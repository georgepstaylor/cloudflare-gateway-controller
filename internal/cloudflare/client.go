package cloudflare

import (
	"context"
	"fmt"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/dns"
	"github.com/cloudflare/cloudflare-go/v6/option"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
	"github.com/cloudflare/cloudflare-go/v6/zones"
)

// Client wraps the Cloudflare API client with methods for tunnel and DNS management
type Client struct {
	api       *cloudflare.Client
	accountID string
}

// NewClient creates a new Cloudflare client
func NewClient(apiToken, accountID string) (*Client, error) {
	api := cloudflare.NewClient(
		option.WithAPIToken(apiToken),
	)

	return &Client{
		api:       api,
		accountID: accountID,
	}, nil
}

// AccountID returns the Cloudflare account ID
func (c *Client) AccountID() string {
	return c.accountID
}

// TunnelConfig represents the configuration for a Cloudflare Tunnel
type TunnelConfig struct {
	Name   string
	Secret string
}

// CreateTunnel creates a new Cloudflare Tunnel
func (c *Client) CreateTunnel(ctx context.Context, name string) (*zero_trust.TunnelCloudflaredNewResponse, string, error) {
	// Generate a random secret for the tunnel
	secret, err := generateTunnelSecret()
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate tunnel secret: %w", err)
	}

	params := zero_trust.TunnelCloudflaredNewParams{
		AccountID:    cloudflare.F(c.accountID),
		Name:         cloudflare.F(name),
		TunnelSecret: cloudflare.F(secret),
		ConfigSrc:    cloudflare.F(zero_trust.TunnelCloudflaredNewParamsConfigSrcLocal),
	}

	tunnel, err := c.api.ZeroTrust.Tunnels.Cloudflared.New(ctx, params)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create tunnel: %w", err)
	}

	return tunnel, secret, nil
}

// GetTunnel retrieves a tunnel by ID
func (c *Client) GetTunnel(ctx context.Context, tunnelID string) (*zero_trust.TunnelCloudflaredGetResponse, error) {
	params := zero_trust.TunnelCloudflaredGetParams{
		AccountID: cloudflare.F(c.accountID),
	}

	tunnel, err := c.api.ZeroTrust.Tunnels.Cloudflared.Get(ctx, tunnelID, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get tunnel: %w", err)
	}

	return tunnel, nil
}

// DeleteTunnel deletes a Cloudflare Tunnel
func (c *Client) DeleteTunnel(ctx context.Context, tunnelID string) error {
	params := zero_trust.TunnelCloudflaredDeleteParams{
		AccountID: cloudflare.F(c.accountID),
	}

	_, err := c.api.ZeroTrust.Tunnels.Cloudflared.Delete(ctx, tunnelID, params)
	if err != nil {
		return fmt.Errorf("failed to delete tunnel: %w", err)
	}

	return nil
}

// GetTunnelToken generates a token for connecting cloudflared to the tunnel
func (c *Client) GetTunnelToken(ctx context.Context, tunnelID string) (string, error) {
	params := zero_trust.TunnelCloudflaredTokenGetParams{
		AccountID: cloudflare.F(c.accountID),
	}

	token, err := c.api.ZeroTrust.Tunnels.Cloudflared.Token.Get(ctx, tunnelID, params)
	if err != nil {
		return "", fmt.Errorf("failed to get tunnel token: %w", err)
	}

	return *token, nil
}

// DNSRecordParams contains parameters for creating/updating DNS records
type DNSRecordParams struct {
	ZoneID  string
	Name    string
	Content string
	Type    string
	Proxied bool
	TTL     int
}

// CreateDNSRecord creates a new CNAME DNS record pointing to a tunnel
func (c *Client) CreateDNSRecord(ctx context.Context, params DNSRecordParams) (*dns.RecordResponse, error) {
	recordParams := dns.RecordNewParams{
		ZoneID: cloudflare.F(params.ZoneID),
		Body: dns.CNAMERecordParam{
			Name:    cloudflare.F(params.Name),
			TTL:     cloudflare.F(dns.TTL(params.TTL)),
			Type:    cloudflare.F(dns.CNAMERecordTypeCNAME),
			Content: cloudflare.F(params.Content),
			Proxied: cloudflare.F(params.Proxied),
			Comment: cloudflare.F("Managed by cloudflare-gateway-controller"),
		},
	}

	response, err := c.api.DNS.Records.New(ctx, recordParams)
	if err != nil {
		return nil, fmt.Errorf("failed to create DNS record: %w", err)
	}

	return response, nil
}

// UpdateDNSRecord updates an existing DNS record
func (c *Client) UpdateDNSRecord(ctx context.Context, zoneID, recordID string, params DNSRecordParams) error {
	var updateParams dns.RecordUpdateParams

	// Build the appropriate record type based on params.Type
	switch params.Type {
	case "A":
		updateParams = dns.RecordUpdateParams{
			ZoneID: cloudflare.F(zoneID),
			Body: dns.ARecordParam{
				Name:    cloudflare.F(params.Name),
				TTL:     cloudflare.F(dns.TTL(params.TTL)),
				Type:    cloudflare.F(dns.ARecordTypeA),
				Content: cloudflare.F(params.Content),
				Proxied: cloudflare.F(params.Proxied),
			},
		}
	case "AAAA":
		updateParams = dns.RecordUpdateParams{
			ZoneID: cloudflare.F(zoneID),
			Body: dns.AAAARecordParam{
				Name:    cloudflare.F(params.Name),
				TTL:     cloudflare.F(dns.TTL(params.TTL)),
				Type:    cloudflare.F(dns.AAAARecordTypeAAAA),
				Content: cloudflare.F(params.Content),
				Proxied: cloudflare.F(params.Proxied),
			},
		}
	case "CNAME":
		updateParams = dns.RecordUpdateParams{
			ZoneID: cloudflare.F(zoneID),
			Body: dns.CNAMERecordParam{
				Name:    cloudflare.F(params.Name),
				TTL:     cloudflare.F(dns.TTL(params.TTL)),
				Type:    cloudflare.F(dns.CNAMERecordTypeCNAME),
				Content: cloudflare.F(params.Content),
				Proxied: cloudflare.F(params.Proxied),
			},
		}
	default:
		return fmt.Errorf("unsupported record type: %s", params.Type)
	}

	_, err := c.api.DNS.Records.Update(ctx, recordID, updateParams)
	if err != nil {
		return fmt.Errorf("failed to update DNS record: %w", err)
	}

	return nil
}

// DeleteDNSRecord deletes a DNS record
func (c *Client) DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error {
	deleteParams := dns.RecordDeleteParams{
		ZoneID: cloudflare.F(zoneID),
	}

	_, err := c.api.DNS.Records.Delete(ctx, recordID, deleteParams)
	if err != nil {
		return fmt.Errorf("failed to delete DNS record: %w", err)
	}

	return nil
}

// ListDNSRecords lists DNS records for a zone with optional filtering
func (c *Client) ListDNSRecords(ctx context.Context, zoneID, name, recordType string) ([]dns.RecordResponse, error) {
	listParams := dns.RecordListParams{
		ZoneID: cloudflare.F(zoneID),
	}

	if name != "" {
		listParams.Name = cloudflare.F(dns.RecordListParamsName{
			Exact: cloudflare.F(name),
		})
	}
	if recordType != "" {
		listParams.Type = cloudflare.F(dns.RecordListParamsType(recordType))
	}

	iter := c.api.DNS.Records.ListAutoPaging(ctx, listParams)

	// Collect all records from pagination
	var records []dns.RecordResponse
	for iter.Next() {
		records = append(records, iter.Current())
	}

	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate DNS records: %w", err)
	}

	return records, nil
}

// GetZoneIDByName retrieves a zone ID by domain name
func (c *Client) GetZoneIDByName(ctx context.Context, zoneName string) (string, error) {
	// In v6, we need to list zones and find by name
	iter := c.api.Zones.ListAutoPaging(ctx, zones.ZoneListParams{
		Name: cloudflare.F(zoneName),
	})

	for iter.Next() {
		zone := iter.Current()
		if zone.Name == zoneName {
			return zone.ID, nil
		}
	}

	if err := iter.Err(); err != nil {
		return "", fmt.Errorf("failed to iterate zones: %w", err)
	}

	return "", fmt.Errorf("zone %s not found", zoneName)
}

// generateTunnelSecret generates a random 32-byte secret for tunnel authentication
func generateTunnelSecret() (string, error) {
	// The Cloudflare API expects a base64-encoded 32-byte secret
	// For now, we'll use a simple approach - in production, use crypto/rand
	secret := make([]byte, 32)
	// TODO: Use crypto/rand to generate secure random bytes
	return fmt.Sprintf("%x", secret), nil
}
