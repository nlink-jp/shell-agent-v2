// Package toolcall manages shell script tool registration and execution with MITL.
package toolcall

// Category determines MITL approval requirements.
type Category string

const (
	CategoryRead    Category = "read"
	CategoryWrite   Category = "write"
	CategoryExecute Category = "execute"
)

// Tool represents a registered shell script tool.
type Tool struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Params      []Param  `json:"params"`
	Category    Category `json:"category"`
	ScriptPath  string   `json:"script_path"`
}

// Param is a tool parameter definition.
type Param struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}
