package channels

// ConfigView is the configuration guide exposed by the Web channel. This is a
// local, personal-project tool, so the caller may include complete values,
// including credentials, when the user explicitly enables the wizard.
type ConfigView struct {
	Sections []ConfigSection   `json:"sections"`
	Editable bool              `json:"editable,omitempty"`
	File     string            `json:"file,omitempty"`
	Values   map[string]string `json:"values,omitempty"`
}

type ConfigStore interface {
	Snapshot() map[string]string
	Update(map[string]string) error
}

type ConfigSection struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Items       []ConfigItem `json:"items"`
}

type ConfigItem struct {
	Key             string `json:"key"`
	Env             string `json:"env,omitempty"`
	Value           string `json:"value"`
	Default         string `json:"default,omitempty"`
	Description     string `json:"description"`
	Status          string `json:"status,omitempty"`
	Sensitive       bool   `json:"sensitive,omitempty"`
	RestartRequired bool   `json:"restart_required"`
}
