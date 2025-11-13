package cloudflare

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

type TLSSettings struct {
	OriginServerName string `yaml:"originServerName,omitempty"`
	MatchSNItoHost   bool   `yaml:"matchSNItoHost,omitempty"`
	CAPool           string `yaml:"caPool,omitempty"`
	NoTLSVerify      bool   `yaml:"noTLSVerify,omitempty"`
	TLSTimeout       string `yaml:"tlsTimeout,omitempty"`
	HTTP2Origin      bool   `yaml:"http2Origin,omitempty"`
}

type HTTPSettings struct {
	HTTPHostHeader         string `yaml:"httpHostHeader,omitempty"`
	DisableChunkedEncoding bool   `yaml:"disableChunkedEncoding,omitempty"`
}

type ConnectionSettings struct {
	ConnectTimeout       string `yaml:"connectTimeout,omitempty"`
	NoHappyEyeballs      bool   `yaml:"noHappyEyeballs,omitempty"`
	ProxyType            string `yaml:"proxyType,omitempty"`
	ProxyAddress         string `yaml:"proxyAddress,omitempty"`
	ProxyPort            int    `yaml:"proxyPort,omitempty"`
	KeepAliveTimeout     string `yaml:"keepAliveTimeout,omitempty"`
	KeepAliveConnections int    `yaml:"keepAliveConnections,omitempty"`
	TCPKeepAlive         bool   `yaml:"tcpKeepAlive,omitempty"`
}

type AccessSettings struct {
	Required bool     `yaml:"required,omitempty"`
	teamName string   `yaml:"teamName,omitempty"`
	AudTag   []string `yaml:"audTag,omitempty"`
}

// OriginRequestConfig represents origin request settings
type OriginRequestConfig struct {
	TLSSettings        `yaml:",inline,omitempty"`
	HTTPSettings       `yaml:",inline,omitempty"`
	ConnectionSettings `yaml:",inline,omitempty"`
	AccessSettings     `yaml:",inline,omitempty"`
}

// IngressRule represents a cloudflared ingress rule
type IngressRule struct {
	Hostname      string               `yaml:"hostname,omitempty"`
	Path          string               `yaml:"path,omitempty"`
	Service       string               `yaml:"service"`
	OriginRequest *OriginRequestConfig `yaml:"originRequest,omitempty"`
}

// TunnelConfiguration represents the full cloudflared configuration
type TunnelConfiguration struct {
	TunnelID        string        `yaml:"tunnel"`
	CredentialsFile string        `yaml:"credentials-file"`
	Ingress         []IngressRule `yaml:"ingress"`
}

// ConfigGenerator generates cloudflared configuration from routing rules
type ConfigGenerator struct{}

// NewConfigGenerator creates a new config generator
func NewConfigGenerator() *ConfigGenerator {
	return &ConfigGenerator{}
}

// GenerateConfig creates a cloudflared configuration YAML from ingress rules
func (g *ConfigGenerator) GenerateConfig(tunnelID string, rules []IngressRule) (string, error) {
	// Ensure there's always a catch-all rule at the end (required by cloudflared)
	if len(rules) == 0 || rules[len(rules)-1].Service != "http_status:404" {
		rules = append(rules, IngressRule{
			Service: "http_status:404",
		})
	}

	config := TunnelConfiguration{
		TunnelID:        tunnelID,
		CredentialsFile: "/etc/cloudflared/credentials.json",
		Ingress:         rules,
	}

	data, err := yaml.Marshal(&config)
	if err != nil {
		return "", fmt.Errorf("failed to marshal config: %w", err)
	}

	return string(data), nil
}

// GenerateCredentialsJSON creates the credentials file content for cloudflared
func (g *ConfigGenerator) GenerateCredentialsJSON(accountID, tunnelID, tunnelSecret string) string {
	return fmt.Sprintf(`{
  "AccountTag": "%s",
  "TunnelID": "%s",
  "TunnelSecret": "%s"
}`, accountID, tunnelID, tunnelSecret)
}

// IngressRuleBuilder helps build ingress rules from HTTPRoute specifications
type IngressRuleBuilder struct {
	rules []IngressRule
}

// NewIngressRuleBuilder creates a new ingress rule builder
func NewIngressRuleBuilder() *IngressRuleBuilder {
	return &IngressRuleBuilder{
		rules: make([]IngressRule, 0),
	}
}

// AddRule adds an ingress rule with optional origin request configuration
func (b *IngressRuleBuilder) AddRule(hostname, path, serviceURL string, originRequest *OriginRequestConfig) *IngressRuleBuilder {
	rule := IngressRule{
		Hostname:      hostname,
		Service:       serviceURL,
		OriginRequest: originRequest,
	}

	if path != "" && path != "/" {
		rule.Path = path
	}

	b.rules = append(b.rules, rule)
	return b
}

// Build returns the constructed ingress rules
func (b *IngressRuleBuilder) Build() []IngressRule {
	return b.rules
}

// ServiceURL constructs an HTTP service URL for cloudflared ingress rules
func ServiceURL(serviceName, namespace string, port int32, clusterDomain string) string {
	return ServiceURLWithScheme("http", serviceName, namespace, port, clusterDomain)
}

// ServiceURLWithScheme constructs a service URL with the specified scheme (http/https)
func ServiceURLWithScheme(scheme, serviceName, namespace string, port int32, clusterDomain string) string {
	// Use Kubernetes cluster DNS format with configurable domain
	return fmt.Sprintf("%s://%s.%s.svc.%s:%d", scheme, serviceName, namespace, clusterDomain, port)
}
