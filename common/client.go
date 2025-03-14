package common

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/api/option"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/mitchellh/go-homedir"
	"gopkg.in/ini.v1"
)

// Default settings
const (
	DefaultTruncateBytes      = 96
	DefaultRateLimitPerSecond = 15
	DefaultHTTPTimeoutSeconds = 60
)

// DatabricksClient is the client struct that contains clients for all the services available on Databricks
type DatabricksClient struct {
	Host               string
	Token              string
	Username           string
	Password           string
	Profile            string
	ConfigFile         string
	AccountID          string
	AzureAuth          AzureAuth
	InsecureSkipVerify bool
	DevelopmentMode    bool
	HTTPTimeoutSeconds int
	DebugTruncateBytes int
	DebugHeaders       bool
	RateLimitPerSecond int

	GoogleServiceAccount string
	googleAuthOptions    []option.ClientOption

	// Context from `ConfigureContextFunc` that is
	// to be re-used with OAuth token exchanges
	InitContext context.Context

	authMutex      sync.Mutex
	rateLimiter    *rate.Limiter
	Provider       *schema.Provider
	httpClient     *retryablehttp.Client
	authVisitor    func(r *http.Request) error
	commandFactory func(context.Context, *DatabricksClient) CommandExecutor
}

// Configure client to work
func (c *DatabricksClient) Configure() error {
	c.configureHTTPCLient()
	c.AzureAuth.databricksClient = c
	if c.DebugTruncateBytes == 0 {
		c.DebugTruncateBytes = DefaultTruncateBytes
	}
	return nil
}

// Authenticate authenticates across providers or returns error
func (c *DatabricksClient) Authenticate() error {
	if c.authVisitor != nil {
		return nil
	}
	c.authMutex.Lock()
	defer c.authMutex.Unlock()
	if c.authVisitor != nil {
		return nil
	}
	authorizers := []func() (func(r *http.Request) error, error){
		c.configureAuthWithDirectParams,
		c.AzureAuth.configureWithClientSecret,
		c.AzureAuth.configureWithAzureCLI,
		c.configureWithGoogleForAccountsAPI,
		c.configureWithGoogleForWorkspace,
		c.configureFromDatabricksCfg,
	}
	for _, authProvider := range authorizers {
		authorizer, err := authProvider()
		if err != nil {
			return err
		}
		if authorizer == nil {
			continue
		}
		c.authVisitor = authorizer
		c.fixHost()
		return nil
	}
	return fmt.Errorf("authentication is not configured for provider. Please configure it\n" +
		"through one of the following options:\n" +
		"1. DATABRICKS_HOST + DATABRICKS_TOKEN environment variables.\n" +
		"2. host + token provider arguments.\n" +
		"3. azure_databricks_workspace_id + AZ CLI authentication.\n" +
		"4. azure_databricks_workspace_id + azure_client_id + azure_client_secret + azure_tenant_id " +
		"for Azure Service Principal authentication.\n" +
		"5. Run `databricks configure --token` that will create ~/.databrickscfg file.\n\n" +
		"Please check https://registry.terraform.io/providers/databrickslabs/databricks/latest/docs#authentication for details")
}

func (c *DatabricksClient) fixHost() {
	if c.Host != "" && !(strings.HasPrefix(c.Host, "https://") || strings.HasPrefix(c.Host, "http://")) {
		// azurerm_databricks_workspace.*.workspace_url is giving URL without scheme
		// so that is why this line is here
		c.Host = "https://" + c.Host
	}
}

func (c *DatabricksClient) configureAuthWithDirectParams() (func(r *http.Request) error, error) {
	authType := "Bearer"
	var needsHostBecause string
	if c.Username != "" && c.Password != "" {
		authType = "Basic"
		needsHostBecause = "basic_auth"
		c.Token = c.encodeBasicAuth(c.Username, c.Password)
		c.Password = ""
		log.Printf("[INFO] Using basic auth for user '%s'", c.Username)
	} else if c.Token != "" {
		needsHostBecause = "token"
	}
	if needsHostBecause != "" && c.Host == "" {
		return nil, fmt.Errorf("host is empty, but is required by %s", needsHostBecause)
	}
	if c.Token == "" || c.Host == "" {
		return nil, nil
	}
	log.Printf("[INFO] Using directly configured host+%s authentication", needsHostBecause)
	return c.authorizer(authType, c.Token), nil
}

func (c *DatabricksClient) configureFromDatabricksCfg() (func(r *http.Request) error, error) {
	configFile := c.ConfigFile
	if configFile == "" {
		configFile = "~/.databrickscfg"
	}
	configFile, err := homedir.Expand(configFile)
	if err != nil {
		return nil, err
	}
	_, err = os.Stat(configFile)
	if os.IsNotExist(err) {
		log.Printf("[INFO] ~/.databrickscfg not found on current host")
		// early return for non-configured machines
		return nil, nil
	}
	cfg, err := ini.Load(configFile)
	if err != nil {
		return nil, err
	}
	if c.Profile == "" {
		log.Printf("[INFO] Using DEFAULT profile from %s", configFile)
		c.Profile = "DEFAULT"
	}
	dbcli := cfg.Section(c.Profile)
	if len(dbcli.Keys()) == 0 {
		// here we meet a heavy user of Databricks CLI
		return nil, fmt.Errorf("%s has no %s profile configured", configFile, c.Profile)
	}
	c.Host = dbcli.Key("host").String()
	if c.Host == "" {
		return nil, fmt.Errorf("config file %s is corrupt: cannot find host in %s profile",
			configFile, c.Profile)
	}
	authType := "Bearer"
	if dbcli.HasKey("username") && dbcli.HasKey("password") {
		username := dbcli.Key("username").String()
		password := dbcli.Key("password").String()
		c.Token = c.encodeBasicAuth(username, password)
		authType = "Basic"
	} else {
		c.Token = dbcli.Key("token").String()
	}
	if c.Token == "" {
		return nil, fmt.Errorf("config file %s is corrupt: cannot find token in %s profile",
			configFile, c.Profile)
	}
	log.Printf("[INFO] Using %s authentication from ~/.databrickscfg", authType)
	return c.authorizer(authType, c.Token), nil
}

func (c *DatabricksClient) authorizer(authType, token string) func(r *http.Request) error {
	return func(r *http.Request) error {
		r.Header.Set("Authorization", fmt.Sprintf("%s %s", authType, token))
		return nil
	}
}

func (c *DatabricksClient) encodeBasicAuth(username, password string) string {
	tokenUnB64 := fmt.Sprintf("%s:%s", username, password)
	return base64.StdEncoding.EncodeToString([]byte(tokenUnB64))
}

func (c *DatabricksClient) configureHTTPCLient() {
	if c.HTTPTimeoutSeconds == 0 {
		c.HTTPTimeoutSeconds = DefaultHTTPTimeoutSeconds
	}
	if c.RateLimitPerSecond == 0 {
		c.RateLimitPerSecond = DefaultRateLimitPerSecond
	}
	if c.InitContext == nil {
		c.InitContext = context.Background()
	}
	c.rateLimiter = rate.NewLimiter(rate.Limit(c.RateLimitPerSecond), 1)
	// Set up a retryable HTTP Client to handle cases where the service returns
	// a transient error on initial creation
	retryDelayDuration := 10 * time.Second
	retryMaximumDuration := 5 * time.Minute
	defaultTransport := http.DefaultTransport.(*http.Transport)
	c.httpClient = &retryablehttp.Client{
		HTTPClient: &http.Client{
			Timeout: time.Duration(c.HTTPTimeoutSeconds) * time.Second,
			Transport: &http.Transport{
				Proxy:                 defaultTransport.Proxy,
				DialContext:           defaultTransport.DialContext,
				MaxIdleConns:          defaultTransport.MaxIdleConns,
				IdleConnTimeout:       defaultTransport.IdleConnTimeout * 3,
				TLSHandshakeTimeout:   defaultTransport.TLSHandshakeTimeout * 3,
				ExpectContinueTimeout: defaultTransport.ExpectContinueTimeout,
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: c.InsecureSkipVerify,
				},
			},
		},
		CheckRetry: c.checkHTTPRetry,
		// Using a linear retry rather than the default exponential retry
		// as the creation condition is normally passed after 30-40 seconds
		// Setting the retry interval to 10 seconds. Setting RetryWaitMin and RetryWaitMax
		// to the same value removes jitter (which would be useful in a high-volume traffic scenario
		// but wouldn't add much here)
		Backoff:      retryablehttp.LinearJitterBackoff,
		RetryWaitMin: retryDelayDuration,
		RetryWaitMax: retryDelayDuration,
		RetryMax:     int(retryMaximumDuration / retryDelayDuration),
	}
}

// IsAzure returns true if client is configured for Azure Databricks - either by using AAD auth or with host+token combination
func (c *DatabricksClient) IsAzure() bool {
	return c.AzureAuth.resourceID() != "" || strings.Contains(c.Host, ".azuredatabricks.net")
}

// IsAws returns true if client is configured for AWS
func (c *DatabricksClient) IsAws() bool {
	return !c.IsAzure() && !c.IsGcp()
}

// IsGcp returns true if client is configured for GCP
func (c *DatabricksClient) IsGcp() bool {
	return strings.Contains(c.Host, ".gcp.databricks.com")
}

// FormatURL creates URL from the client Host and additional strings
func (c *DatabricksClient) FormatURL(strs ...string) string {
	host := c.Host
	if !strings.HasSuffix(host, "/") {
		host += "/"
	}
	data := append([]string{host}, strs...)
	return strings.Join(data, "")
}
