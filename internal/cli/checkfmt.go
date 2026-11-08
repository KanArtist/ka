package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// Output formats for ka check (PRD §16: GitHub annotations, SARIF or JSON).

// diagFile maps a diagnostic's label-prefixed path to a file path relative
// to base (the checkout root in CI), slash-separated.
func diagFile(ws *kaspec.Workspace, d kaspec.Diagnostic, base string) string {
	if ws == nil {
		return d.Path
	}
	best, root, rel := -1, "", d.Path
	for _, s := range ws.Stores {
		key := ws.KeyOf(s)
		if key == "" {
			continue
		}
		if s.Label == "" {
			if best < 0 {
				best, root, rel = 0, key, d.Path
			}
			continue
		}
		if strings.HasPrefix(d.Path, s.Label+"/") && len(s.Label) > best {
			best, root, rel = len(s.Label), key, strings.TrimPrefix(d.Path, s.Label+"/")
		}
	}
	if root == "" {
		return d.Path
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if base != "" {
		if r, err := filepath.Rel(base, abs); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
		}
	}
	return filepath.ToSlash(abs)
}

// writeGitHub prints workflow commands that GitHub shows as annotations on
// the files and lines.
func writeGitHub(w io.Writer, ws *kaspec.Workspace, diags []kaspec.Diagnostic, base string) {
	for _, d := range diags {
		level := map[kaspec.Severity]string{kaspec.SevError: "error", kaspec.SevWarning: "warning"}[d.Severity]
		if level == "" {
			level = "notice"
		}
		props := "file=" + ghProp(diagFile(ws, d, base))
		if d.Line > 0 {
			props += fmt.Sprintf(",line=%d", d.Line)
		}
		props += ",title=" + ghProp("ka check: "+d.Code)
		msg := d.Message
		if d.Fix != "" {
			msg += "\nfix: " + d.Fix
		}
		fmt.Fprintf(w, "::%s %s::%s\n", level, props, ghData(msg))
	}
}

func ghData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

func ghProp(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(s)
}

// SARIF 2.1.0, for code scanning.
type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version,omitempty"`
	InformationURI string      `json:"informationUri"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string       `json:"id"`
	ShortDescription sarifMessage `json:"shortDescription"`
	DefaultConfig    struct {
		Level string `json:"level"`
	} `json:"defaultConfiguration"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID    string          `json:"ruleId"`
	Level     string          `json:"level"`
	Message   sarifMessage    `json:"message"`
	Locations []sarifLocation `json:"locations"`
}

type sarifLocation struct {
	Physical struct {
		Artifact struct {
			URI string `json:"uri"`
		} `json:"artifactLocation"`
		Region *struct {
			StartLine int `json:"startLine"`
		} `json:"region,omitempty"`
	} `json:"physicalLocation"`
}

func sarifLevel(s kaspec.Severity) string {
	switch s {
	case kaspec.SevError:
		return "error"
	case kaspec.SevWarning:
		return "warning"
	}
	return "note"
}

func writeSARIF(w io.Writer, ws *kaspec.Workspace, diags []kaspec.Diagnostic, base, version string) error {
	run := sarifRun{Tool: sarifTool{Driver: sarifDriver{Name: "ka", Version: version,
		InformationURI: "https://github.com/JumpKickOSS/ka/blob/main/spec/v1/README.md", Rules: []sarifRule{}}}, Results: []sarifResult{}}
	seen := map[string]bool{}
	for _, d := range diags {
		if !seen[d.Code] {
			seen[d.Code] = true
			r := sarifRule{ID: d.Code, ShortDescription: sarifMessage{Text: strings.ReplaceAll(d.Code, "_", " ")}}
			r.DefaultConfig.Level = sarifLevel(kaspec.Codes[d.Code])
			run.Tool.Driver.Rules = append(run.Tool.Driver.Rules, r)
		}
		msg := d.Message
		if d.Fix != "" {
			msg += " (fix: " + d.Fix + ")"
		}
		var loc sarifLocation
		loc.Physical.Artifact.URI = diagFile(ws, d, base)
		if d.Line > 0 {
			loc.Physical.Region = &struct {
				StartLine int `json:"startLine"`
			}{d.Line}
		}
		run.Results = append(run.Results, sarifResult{RuleID: d.Code, Level: sarifLevel(d.Severity), Message: sarifMessage{Text: msg}, Locations: []sarifLocation{loc}})
	}
	sort.Slice(run.Tool.Driver.Rules, func(i, j int) bool { return run.Tool.Driver.Rules[i].ID < run.Tool.Driver.Rules[j].ID })
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(sarifLog{Schema: "https://json.schemastore.org/sarif-2.1.0.json", Version: "2.1.0", Runs: []sarifRun{run}})
}
