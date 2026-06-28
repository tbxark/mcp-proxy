package main

import (
	"encoding/json"
	"testing"
)

func TestToolFilterConfigUnmarshalJSON(t *testing.T) {
	t.Parallel()

	t.Run("array list", func(t *testing.T) {
		var cfg ToolFilterConfig
		if err := json.Unmarshal([]byte(`{
			"mode": "allow",
			"list": ["jira_get_issue", "jira_search"]
		}`), &cfg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if cfg.Mode != ToolFilterModeAllow {
			t.Fatalf("mode = %q, want allow", cfg.Mode)
		}
		if len(cfg.List) != 2 {
			t.Fatalf("list = %#v, want 2 entries", cfg.List)
		}
	})

	t.Run("comma-separated string list", func(t *testing.T) {
		var cfg ToolFilterConfig
		if err := json.Unmarshal([]byte(`{
			"mode": "allow",
			"list": "confluence_search,jira_get_issue,jira_search"
		}`), &cfg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got := normalizeToolFilterList(cfg.List)
		want := []string{"confluence_search", "jira_get_issue", "jira_search"}
		if len(got) != len(want) {
			t.Fatalf("normalized list = %#v, want %#v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("normalized list = %#v, want %#v", got, want)
			}
		}
	})
}

func TestNormalizeToolFilterList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "comma separated single entry",
			in:   []string{"confluence_search,jira_get_issue,jira_search"},
			want: []string{"confluence_search", "jira_get_issue", "jira_search"},
		},
		{
			name: "trim spaces",
			in:   []string{" jira_get_issue ", "jira_search"},
			want: []string{"jira_get_issue", "jira_search"},
		},
		{
			name: "dedupe",
			in:   []string{"jira_search", "jira_search"},
			want: []string{"jira_search"},
		},
		{
			name: "mixed array and comma separated",
			in:   []string{"confluence_search", "jira_get_issue,jira_search"},
			want: []string{"confluence_search", "jira_get_issue", "jira_search"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeToolFilterList(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("got %#v, want %#v", got, tt.want)
				}
			}
		})
	}
}

func TestBuildToolFilterFuncAllowList(t *testing.T) {
	t.Parallel()

	filter := buildToolFilterFunc("atlassian", &OptionsV2{
		ToolFilter: &ToolFilterConfig{
			Mode: ToolFilterModeAllow,
			List: []string{"confluence_search,jira_get_issue,jira_search"},
		},
	})

	for _, allowed := range []string{"confluence_search", "jira_get_issue", "jira_search"} {
		if !filter(allowed) {
			t.Fatalf("expected %q to be allowed", allowed)
		}
	}
	if filter("jira_create_issue") {
		t.Fatal("expected jira_create_issue to be blocked")
	}
}
