package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	extapi "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/jetstack/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	"github.com/jetstack/cert-manager/pkg/acme/webhook/cmd"
	"github.com/jetstack/cert-manager/pkg/issuer/acme/dns/util"

	"github.com/kaelzhang/dnspod-go"
)

const (
	defaultTTL = 600
)

var groupName = os.Getenv("GROUP_NAME")

func main() {
	if groupName == "" {
		panic("GROUP_NAME must be specified")
	}

	// This will register our custom DNS provider with the webhook serving
	// library, making it available as an API under the provided groupName.
	// You can register multiple DNS provider implementations with a single
	// webhook, where the Name() method will be used to disambiguate between
	// the different implementations.
	cmd.RunWebhookServer(groupName,
		&customDNSProviderSolver{},
	)
}

type cachedDnspodClient struct {
	Client        *dnspod.Client
	SecretVersion string
}

// customDNSProviderSolver implements the provider-specific logic needed to
// 'present' an ACME challenge TXT record for your own DNS provider.
// To do so, it must implement the `github.com/jetstack/cert-manager/pkg/acme/webhook.Solver`
// interface.
type customDNSProviderSolver struct {
	client *kubernetes.Clientset
	dnspod map[string]cachedDnspodClient
}

type apiTokenSecretRef struct {
	name      string
	namespace string
}

// customDNSProviderConfig is a structure that is used to decode into when
// solving a DNS01 challenge.
// This information is provided by cert-manager, and may be a reference to
// additional configuration that's needed to solve the challenge for this
// particular certificate or issuer.
// This typically includes references to Secret resources containing DNS
// provider credentials, in cases where a 'multi-tenant' DNS solver is being
// created.
// If you do *not* require per-issuer or per-certificate configuration to be
// provided to your webhook, you can skip decoding altogether in favour of
// using CLI flags or similar to provide configuration.
// You should not include sensitive information here. If credentials need to
// be used by your provider here, you should reference a Kubernetes Secret
// resource and fetch these credentials using a Kubernetes clientset.
type customDNSProviderConfig struct {
	APITokenSecret apiTokenSecretRef `json:"apiTokenSecret"`
	TTL            int               `json:"ttl"`
}

// Name is used as the name for this DNS solver when referencing it on the ACME
// Issuer resource.
// This should be unique **within the group name**, i.e. you can have two
// solvers configured with the same Name() **so long as they do not co-exist
// within a single webhook deployment**.
func (c *customDNSProviderSolver) Name() string {
	return "dnspodchallenger"
}

// Present is responsible for actually presenting the DNS record with the
// DNS provider.
// This method should tolerate being called multiple times with the same value.
// cert-manager itself will later perform a self check to ensure that the
// solver has correctly configured the DNS provider.
func (c *customDNSProviderSolver) Present(ch *v1alpha1.ChallengeRequest) error {
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return err
	}

	dnspodClient, err := c.getDNSPod(ch, cfg)
	if err != nil {
		return err
	}

	domainID, err := getDomainID(dnspodClient, ch.ResolvedZone)
	if err != nil {
		return err
	}

	recordAttributes := newTxtRecord(ch.ResolvedZone, ch.ResolvedFQDN, ch.Key, cfg.TTL)
	_, _, err = dnspodClient.Domains.CreateRecord(domainID, *recordAttributes)
	if err != nil {
		return fmt.Errorf("dnspod API call failed: %v", err)
	}

	return nil
}

// CleanUp should delete the relevant TXT record from the DNS provider console.
// If multiple TXT records exist with the same record name (e.g.
// _acme-challenge.example.com) then **only** the record with the same `key`
// value provided on the ChallengeRequest should be cleaned up.
// This is in order to facilitate multiple DNS validations for the same domain
// concurrently.
func (c *customDNSProviderSolver) CleanUp(ch *v1alpha1.ChallengeRequest) error {
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return err
	}

	dnspodClient, err := c.getDNSPod(ch, cfg)
	if err != nil {
		return err
	}

	domainID, err := getDomainID(dnspodClient, ch.ResolvedZone)
	if err != nil {
		return err
	}

	records, err := findTxtRecords(dnspodClient, domainID, ch.ResolvedZone, ch.ResolvedFQDN)
	if err != nil && !strings.Contains(err.Error(), "No records") {
		return err
	}

	for _, record := range records {
		if record.Value != ch.Key {
			continue
		}

		_, err := dnspodClient.Domains.DeleteRecord(domainID, record.ID)
		if err != nil {
			return err
		}
	}

	return nil
}

// Initialize will be called when the webhook first starts.
// This method can be used to instantiate the webhook, i.e. initialising
// connections or warming up caches.
// Typically, the kubeClientConfig parameter is used to build a Kubernetes
// client that can be used to fetch resources from the Kubernetes API, e.g.
// Secret resources containing credentials used to authenticate with DNS
// provider accounts.
// The stopCh can be used to handle early termination of the webhook, in cases
// where a SIGTERM or similar signal is sent to the webhook process.
func (c *customDNSProviderSolver) Initialize(kubeClientConfig *rest.Config, stopCh <-chan struct{}) error {
	cl, err := kubernetes.NewForConfig(kubeClientConfig)
	if err != nil {
		return err
	}
	c.client = cl
	c.dnspod = make(map[string]cachedDnspodClient)

	return nil
}

func (c *customDNSProviderSolver) getDNSPod(ch *v1alpha1.ChallengeRequest, cfg customDNSProviderConfig) (*dnspod.Client, error) {
	secretNS := cfg.APITokenSecret.namespace
	secretName := cfg.APITokenSecret.name
	secretFQN := fmt.Sprintf("%s/%s", secretNS, secretName)

	cached, ok := c.dnspod[secretFQN]
	secret, err := c.client.CoreV1().Secrets(secretNS).Get(secretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if !ok || cached.SecretVersion != secret.ResourceVersion {
		apiId, ok := secret.Data["id"]
		if !ok {
			return nil, fmt.Errorf("no `id` in secret '%s'", secretFQN)
		}
		apiToken, ok := secret.Data["token"]
		if !ok {
			return nil, fmt.Errorf("no `token` in secret '%s'", secretFQN)
		}

		key := fmt.Sprintf("%s,%s", apiId, apiToken)
		params := dnspod.CommonParams{LoginToken: key, Format: "json"}
		dnspodClient := dnspod.NewClient(params)
		cached = cachedDnspodClient{
			Client:        dnspodClient,
			SecretVersion: secret.ResourceVersion,
		}
		c.dnspod[secretFQN] = cached
	}

	return cached.Client, nil
}

// loadConfig is a small helper function that decodes JSON configuration into
// the typed config struct.
func loadConfig(cfgJSON *extapi.JSON) (customDNSProviderConfig, error) {
	cfg := customDNSProviderConfig{
		APITokenSecret: apiTokenSecretRef{
			name:      "dnspod-credentials",
			namespace: "default",
		},
		TTL: defaultTTL,
	}
	// handle the 'base case' where no configuration has been provided
	if cfgJSON == nil {
		return cfg, nil
	}
	if err := json.Unmarshal(cfgJSON.Raw, &cfg); err != nil {
		return cfg, fmt.Errorf("error decoding solver config: %v", err)
	}

	return cfg, nil
}

func getDomainID(client *dnspod.Client, zone string) (string, error) {
	domains, _, err := client.Domains.List()
	if err != nil {
		return "", fmt.Errorf("dnspod API call failed: %v", err)
	}

	authZone, err := util.FindZoneByFqdn(zone, util.RecursiveNameservers)
	if err != nil {
		return "", err
	}

	var hostedDomain dnspod.Domain
	for _, domain := range domains {
		if domain.Name == util.UnFqdn(authZone) {
			hostedDomain = domain
			break
		}
	}

	hostedDomainID, err := hostedDomain.ID.Int64()
	if err != nil {
		return "", err
	}
	if hostedDomainID == 0 {
		return "", fmt.Errorf("Zone %s not found in dnspod for zone %s", authZone, zone)
	}

	return fmt.Sprintf("%d", hostedDomainID), nil
}

func newTxtRecord(zone, fqdn, value string, ttl int) *dnspod.Record {
	name := extractRecordName(fqdn, zone)

	return &dnspod.Record{
		Type:  "TXT",
		Name:  name,
		Value: value,
		Line:  "默认",
		TTL:   fmt.Sprintf("%d", ttl),
	}
}

func findTxtRecords(client *dnspod.Client, domainID, zone, fqdn string) ([]dnspod.Record, error) {
	recordName := extractRecordName(fqdn, zone)
	records, _, err := client.Domains.ListRecords(domainID, recordName)
	if err != nil {
		return records, fmt.Errorf("dnspod API call has failed: %v", err)
	}

	return records, nil
}

func extractRecordName(fqdn, zone string) string {
	if idx := strings.Index(fqdn, "."+zone); idx != -1 {
		return fqdn[:idx]
	}

	return util.UnFqdn(fqdn)
}
