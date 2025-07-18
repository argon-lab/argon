package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Client represents the HTTP client for the Argon API
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	APIKey     string
}

// NewClient creates a new API client
func NewClient(baseURL string) *Client {
	apiKey := os.Getenv("ARGON_API_KEY")
	if apiKey == "" {
		apiKey = "dev-api-key" // Default for development
	}

	return &Client{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		APIKey: apiKey,
	}
}

// APIResponse represents a generic API response
type APIResponse struct {
	Data    interface{} `json:"data,omitempty"`
	Message string      `json:"message,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// Project represents a project response from the API
type Project struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	MongodbURI  string            `json:"mongodb_uri,omitempty"`
	Status      string            `json:"status"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	BranchCount int               `json:"branch_count"`
	StorageSize int64             `json:"storage_size"`
}

// Branch represents a branch response from the API
type Branch struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	Status        string    `json:"status"`
	IsMain        bool      `json:"is_main"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	StoragePath   string    `json:"storage_path"`
	DocumentCount int       `json:"document_count"`
	StorageSize   int64     `json:"storage_size"`
}

// ConnectionString represents a connection string response
type ConnectionString struct {
	ConnectionString string     `json:"connection_string"`
	DatabaseName     string     `json:"database_name"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

// BranchStats represents branch statistics
type BranchStats struct {
	DocumentCount    int        `json:"document_count"`
	StorageSize      int64      `json:"storage_size"`
	ChangeCount      int64      `json:"change_count"`
	CompressionRatio float64    `json:"compression_ratio"`
	LastChangeAt     *time.Time `json:"last_change_at"`
}

// makeRequest makes an HTTP request to the API
func (c *Client) makeRequest(method, path string, body interface{}) (*http.Response, error) {
	url := c.BaseURL + path

	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("error marshaling request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonBody)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}

	// Add headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("User-Agent", "argon-cli/2.0.0")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error making request: %w", err)
	}

	return resp, nil
}

// parseResponse parses the HTTP response into the target interface
func (c *Client) parseResponse(resp *http.Response, target interface{}) error {
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		var apiErr APIResponse
		if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error != "" {
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, apiErr.Error)
		}
		return fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(body))
	}

	if target != nil {
		if err := json.Unmarshal(body, target); err != nil {
			return fmt.Errorf("error parsing response: %w", err)
		}
	}

	return nil
}

// Project API methods

// ListProjects retrieves all projects
func (c *Client) ListProjects() ([]Project, error) {
	resp, err := c.makeRequest("GET", "/api/projects", nil)
	if err != nil {
		return nil, err
	}

	var projects []Project
	if err := c.parseResponse(resp, &projects); err != nil {
		return nil, err
	}

	return projects, nil
}

// CreateProject creates a new project
func (c *Client) CreateProject(name, description, mongodbURI string) (*Project, error) {
	body := map[string]string{
		"name":        name,
		"description": description,
		"mongodb_uri": mongodbURI,
	}

	resp, err := c.makeRequest("POST", "/api/projects", body)
	if err != nil {
		return nil, err
	}

	var project Project
	if err := c.parseResponse(resp, &project); err != nil {
		return nil, err
	}

	return &project, nil
}

// GetProject retrieves a specific project
func (c *Client) GetProject(projectID string) (*Project, error) {
	resp, err := c.makeRequest("GET", "/api/projects/"+projectID, nil)
	if err != nil {
		return nil, err
	}

	var project Project
	if err := c.parseResponse(resp, &project); err != nil {
		return nil, err
	}

	return &project, nil
}

// DeleteProject deletes a project
func (c *Client) DeleteProject(projectID string) error {
	resp, err := c.makeRequest("DELETE", "/api/projects/"+projectID, nil)
	if err != nil {
		return err
	}

	return c.parseResponse(resp, nil)
}

// Branch API methods

// ListBranches retrieves all branches for a project
func (c *Client) ListBranches(projectID string) ([]Branch, error) {
	resp, err := c.makeRequest("GET", "/api/projects/"+projectID+"/branches", nil)
	if err != nil {
		return nil, err
	}

	var response struct {
		Branches []Branch `json:"branches"`
	}
	if err := c.parseResponse(resp, &response); err != nil {
		return nil, err
	}

	return response.Branches, nil
}

// CreateBranch creates a new branch
func (c *Client) CreateBranch(projectID, name, description, parentBranch string) (*Branch, error) {
	body := map[string]interface{}{
		"name":        name,
		"description": description,
	}
	if parentBranch != "" {
		body["parent_branch"] = parentBranch
	}

	resp, err := c.makeRequest("POST", "/api/projects/"+projectID+"/branches", body)
	if err != nil {
		return nil, err
	}

	var branch Branch
	if err := c.parseResponse(resp, &branch); err != nil {
		return nil, err
	}

	return &branch, nil
}

// GetBranch retrieves a specific branch
func (c *Client) GetBranch(projectID, branchID string) (*Branch, error) {
	resp, err := c.makeRequest("GET", "/api/projects/"+projectID+"/branches/"+branchID, nil)
	if err != nil {
		return nil, err
	}

	var branch Branch
	if err := c.parseResponse(resp, &branch); err != nil {
		return nil, err
	}

	return &branch, nil
}

// DeleteBranch deletes a branch
func (c *Client) DeleteBranch(projectID, branchID string) error {
	resp, err := c.makeRequest("DELETE", "/api/projects/"+projectID+"/branches/"+branchID, nil)
	if err != nil {
		return err
	}

	return c.parseResponse(resp, nil)
}

// GetBranchStats retrieves branch statistics
func (c *Client) GetBranchStats(projectID, branchID string) (*BranchStats, error) {
	resp, err := c.makeRequest("GET", "/api/projects/"+projectID+"/branches/"+branchID+"/stats", nil)
	if err != nil {
		return nil, err
	}

	var stats BranchStats
	if err := c.parseResponse(resp, &stats); err != nil {
		return nil, err
	}

	return &stats, nil
}

// GetConnectionString retrieves the connection string for a branch
func (c *Client) GetConnectionString(projectID, branchID string) (*ConnectionString, error) {
	resp, err := c.makeRequest("GET", "/api/projects/"+projectID+"/branches/"+branchID+"/connection", nil)
	if err != nil {
		return nil, err
	}

	var connStr ConnectionString
	if err := c.parseResponse(resp, &connStr); err != nil {
		return nil, err
	}

	return &connStr, nil
}

// GetProjectConnectionURI retrieves connection URI (Neon compatibility)
func (c *Client) GetProjectConnectionURI(projectID, branchID string) (string, error) {
	path := "/api/projects/" + projectID + "/connection_uri"
	if branchID != "" {
		path += "?branch_id=" + branchID
	}

	resp, err := c.makeRequest("GET", path, nil)
	if err != nil {
		return "", err
	}

	var response struct {
		URI        string `json:"uri"`
		Database   string `json:"database"`
		BranchID   string `json:"branch_id"`
	}
	if err := c.parseResponse(resp, &response); err != nil {
		return "", err
	}

	return response.URI, nil
}

// Health check
func (c *Client) Health() (map[string]interface{}, error) {
	resp, err := c.makeRequest("GET", "/health", nil)
	if err != nil {
		return nil, err
	}

	var health map[string]interface{}
	if err := c.parseResponse(resp, &health); err != nil {
		return nil, err
	}

	return health, nil
}