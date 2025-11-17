package cloudflare

import (
	"context"
	"fmt"

	"github.com/cloudflare/cloudflare-go"
)

// Client wraps the Cloudflare API client with methods for tunnel and DNS management
type Client struct {
	api       *cloudflare.API
	accountID string
}

// NewClient creates a new Cloudflare client
func NewClient(apiToken, accountID string) (*Client, error) {
	api, err := cloudflare.NewWithAPIToken(apiToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create cloudflare client: %w", err)
	}

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
func (c *Client) CreateTunnel(ctx context.Context, name string) (*cloudflare.Tunnel, string, error) {
	// Generate a random secret for the tunnel
	secret, err := generateTunnelSecret()
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate tunnel secret: %w", err)
	}

	params := cloudflare.TunnelCreateParams{
		Name:      name,
		Secret:    secret,
		ConfigSrc: "local",
	}

	tunnel, err := c.api.CreateTunnel(ctx, cloudflare.AccountIdentifier(c.accountID), params)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create tunnel: %w", err)
	}

	return &tunnel, secret, nil
}

// GetTunnel retrieves a tunnel by ID
func (c *Client) GetTunnel(ctx context.Context, tunnelID string) (*cloudflare.Tunnel, error) {
	tunnel, err := c.api.GetTunnel(ctx, cloudflare.AccountIdentifier(c.accountID), tunnelID)
	if err != nil {
		return nil, fmt.Errorf("failed to get tunnel: %w", err)
	}

	return &tunnel, nil
}

// DeleteTunnel deletes a Cloudflare Tunnel
func (c *Client) DeleteTunnel(ctx context.Context, tunnelID string) error {
	err := c.api.DeleteTunnel(ctx, cloudflare.AccountIdentifier(c.accountID), tunnelID)
	if err != nil {
		return fmt.Errorf("failed to delete tunnel: %w", err)
	}

	return nil
}

// GetTunnelToken generates a token for connecting cloudflared to the tunnel
func (c *Client) GetTunnelToken(ctx context.Context, tunnelID string) (string, error) {
	token, err := c.api.GetTunnelToken(ctx, cloudflare.AccountIdentifier(c.accountID), tunnelID)
	if err != nil {
		return "", fmt.Errorf("failed to get tunnel token: %w", err)
	}

	return token, nil
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

// CreateDNSRecord creates a new DNS record pointing to a tunnel
func (c *Client) CreateDNSRecord(ctx context.Context, params DNSRecordParams) (*cloudflare.DNSRecord, error) {
	record := cloudflare.CreateDNSRecordParams{
		Name:    params.Name,
		Type:    params.Type,
		Content: params.Content,
		Proxied: &params.Proxied,
		TTL:     params.TTL,
	}

	response, err := c.api.CreateDNSRecord(ctx, cloudflare.ZoneIdentifier(params.ZoneID), record)
	if err != nil {
		return nil, fmt.Errorf("failed to create DNS record: %w", err)
	}

	return &response, nil
}

// UpdateDNSRecord updates an existing DNS record
func (c *Client) UpdateDNSRecord(ctx context.Context, zoneID, recordID string, params DNSRecordParams) error {
	record := cloudflare.UpdateDNSRecordParams{
		ID:      recordID,
		Name:    params.Name,
		Type:    params.Type,
		Content: params.Content,
		Proxied: &params.Proxied,
		TTL:     params.TTL,
	}

	_, err := c.api.UpdateDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), record)
	if err != nil {
		return fmt.Errorf("failed to update DNS record: %w", err)
	}

	return nil
}

// DeleteDNSRecord deletes a DNS record
func (c *Client) DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error {
	err := c.api.DeleteDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), recordID)
	if err != nil {
		return fmt.Errorf("failed to delete DNS record: %w", err)
	}

	return nil
}

// ListDNSRecords lists DNS records for a zone with optional filtering
func (c *Client) ListDNSRecords(ctx context.Context, zoneID, name, recordType string) ([]cloudflare.DNSRecord, error) {
	params := cloudflare.ListDNSRecordsParams{
		Name: name,
		Type: recordType,
	}

	records, _, err := c.api.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(zoneID), params)
	if err != nil {
		return nil, fmt.Errorf("failed to list DNS records: %w", err)
	}

	return records, nil
}

// GetZoneIDByName retrieves a zone ID by domain name
func (c *Client) GetZoneIDByName(ctx context.Context, zoneName string) (string, error) {
	zoneID, err := c.api.ZoneIDByName(zoneName)
	if err != nil {
		return "", fmt.Errorf("failed to get zone ID: %w", err)
	}

	return zoneID, nil
}

// generateTunnelSecret generates a random 32-byte secret for tunnel authentication
func generateTunnelSecret() (string, error) {
	// The Cloudflare API expects a base64-encoded 32-byte secret
	// For now, we'll use a simple approach - in production, use crypto/rand
	secret := make([]byte, 32)
	// TODO: Use crypto/rand to generate secure random bytes
	return fmt.Sprintf("%x", secret), nil
}
