// Package workflow parses the subset of GitHub Actions workflow YAML that
// tabrunner currently understands. It is intentionally small: name, jobs,
// steps, run, env. Everything else is ignored for now.
package workflow

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Workflow is a parsed workflow file.
type Workflow struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
	Jobs map[string]*Job   `yaml:"jobs"`
}

// Job is a single job within a workflow.
type Job struct {
	ID     string            `yaml:"-"`
	Name   string            `yaml:"name"`
	RunsOn any               `yaml:"runs-on"`
	Env    map[string]string `yaml:"env"`
	Steps  []*Step           `yaml:"steps"`
}

// Step is a single step within a job. Only `run` steps are supported today.
type Step struct {
	ID              string            `yaml:"id"`
	Name            string            `yaml:"name"`
	Run             string            `yaml:"run"`
	Uses            string            `yaml:"uses"`
	Shell           string            `yaml:"shell"`
	If              string            `yaml:"if"`
	Env             map[string]string `yaml:"env"`
	ContinueOnError bool              `yaml:"continue-on-error"`
}

// Parse reads and parses a workflow file from disk.
func Parse(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes parses workflow YAML from a byte slice.
func ParseBytes(data []byte) (*Workflow, error) {
	var wf Workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow yaml: %w", err)
	}
	if len(wf.Jobs) == 0 {
		return nil, fmt.Errorf("workflow has no jobs")
	}
	for id, job := range wf.Jobs {
		if job == nil {
			return nil, fmt.Errorf("job %q is empty", id)
		}
		job.ID = id
		if job.Name == "" {
			job.Name = id
		}
	}
	return &wf, nil
}

// DisplayName returns the human-friendly label for a step.
func (s *Step) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	if s.Run != "" {
		return firstLine(s.Run)
	}
	if s.Uses != "" {
		return s.Uses
	}
	return "step"
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
