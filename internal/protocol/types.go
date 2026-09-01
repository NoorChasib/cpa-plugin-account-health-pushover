package protocol

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 4

	MethodPluginRegister     = "plugin.register"
	MethodPluginReconfigure  = "plugin.reconfigure"
	MethodPluginQuiesce      = "plugin.quiesce"
	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"
	MethodUsageHandle        = "usage.handle"

	MethodHostLog            = "host.log"
	MethodHostAuthList       = "host.auth.list"
	MethodHostAuthGetRuntime = "host.auth.get_runtime"
)

type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type Registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      Metadata                 `json:"metadata"`
	Capabilities  RegistrationCapabilities `json:"capabilities"`
}

type Metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []ConfigField
}

type ConfigField struct {
	Name        string
	Type        string
	EnumValues  []string
	Description string
}

type RegistrationCapabilities struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

type ManagementRegistration struct {
	Routes    []ManagementRoute `json:"routes,omitempty"`
	Resources []ResourceRoute   `json:"resources,omitempty"`
}

type ManagementRoute struct {
	Method      string
	Path        string
	Menu        string
	Description string
}

type ResourceRoute struct {
	Path        string
	Menu        string
	Description string
}

type ManagementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type ManagementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type HostRecentRequestEntry struct {
	Time    string `json:"time"`
	Success int64  `json:"success"`
	Failed  int64  `json:"failed"`
}

type HostAuthFileEntry struct {
	ID             string                   `json:"id,omitempty"`
	AuthIndex      string                   `json:"auth_index,omitempty"`
	Name           string                   `json:"name"`
	Type           string                   `json:"type,omitempty"`
	Provider       string                   `json:"provider,omitempty"`
	Label          string                   `json:"label,omitempty"`
	Status         string                   `json:"status,omitempty"`
	StatusMessage  string                   `json:"status_message,omitempty"`
	Disabled       bool                     `json:"disabled,omitempty"`
	Unavailable    bool                     `json:"unavailable,omitempty"`
	RuntimeOnly    bool                     `json:"runtime_only,omitempty"`
	Source         string                   `json:"source,omitempty"`
	Path           string                   `json:"path,omitempty"`
	Size           int64                    `json:"size,omitempty"`
	ModTime        time.Time                `json:"modtime,omitempty"`
	UpdatedAt      time.Time                `json:"updated_at,omitempty"`
	CreatedAt      time.Time                `json:"created_at,omitempty"`
	LastRefresh    time.Time                `json:"last_refresh,omitempty"`
	NextRetryAfter time.Time                `json:"next_retry_after,omitempty"`
	Email          string                   `json:"email,omitempty"`
	ProjectID      string                   `json:"project_id,omitempty"`
	AccountType    string                   `json:"account_type,omitempty"`
	Account        string                   `json:"account,omitempty"`
	Priority       int                      `json:"priority,omitempty"`
	Note           string                   `json:"note,omitempty"`
	Websockets     bool                     `json:"websockets,omitempty"`
	Success        int64                    `json:"success,omitempty"`
	Failed         int64                    `json:"failed,omitempty"`
	RecentRequests []HostRecentRequestEntry `json:"recent_requests,omitempty"`
}

type HostAuthListResponse struct {
	Files []HostAuthFileEntry `json:"files"`
}

type HostAuthGetRequest struct {
	AuthIndex string `json:"auth_index"`
}

type HostAuthGetRuntimeResponse struct {
	Auth HostAuthFileEntry `json:"auth"`
}

type HostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level"`
	Message        string         `json:"message"`
	Fields         map[string]any `json:"fields,omitempty"`
}
