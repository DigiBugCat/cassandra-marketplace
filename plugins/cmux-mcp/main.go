package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// --- Host config ---

type HostConfig struct {
	SSH      string `json:"ssh,omitempty"`
	CmuxPath string `json:"cmuxPath,omitempty"`
	Password string `json:"password,omitempty"`
}

var (
	hosts         map[string]HostConfig
	myWorkspace   string
	myWorkspaceMu sync.Mutex
)

const defaultCmuxPath = "/Applications/cmux.app/Contents/Resources/bin/cmux"

func loadHosts() {
	hosts = map[string]HostConfig{"local": {}}
	raw := os.Getenv("CMUX_HOSTS")
	if raw == "" {
		return
	}
	var parsed map[string]HostConfig
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse CMUX_HOSTS: %v\n", err)
		return
	}
	if _, ok := parsed["local"]; !ok {
		parsed["local"] = HostConfig{}
	}
	hosts = parsed
}

func resolveHost(name string) (string, HostConfig, error) {
	if name == "" {
		name = "local"
	}
	cfg, ok := hosts[name]
	if !ok {
		names := make([]string, 0, len(hosts))
		for k := range hosts {
			names = append(names, k)
		}
		return "", HostConfig{}, fmt.Errorf("unknown host %q, available: %s", name, strings.Join(names, ", "))
	}
	return name, cfg, nil
}

func cmuxExec(args []string, hostName string) (string, error) {
	_, cfg, err := resolveHost(hostName)
	if err != nil {
		return "", err
	}

	cmuxPath := cfg.CmuxPath
	if cmuxPath == "" {
		if p := os.Getenv("CMUX_PATH"); p != "" {
			cmuxPath = p
		} else {
			cmuxPath = defaultCmuxPath
		}
	}

	password := cfg.Password
	if password == "" {
		password = os.Getenv("CMUX_SOCKET_PASSWORD")
	}

	cmuxArgs := args
	if password != "" {
		cmuxArgs = append([]string{"--password", password}, args...)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if cfg.SSH != "" {
		// Remote via SSH
		parts := make([]string, 0, len(cmuxArgs)+1)
		parts = append(parts, shellQuote(cmuxPath))
		for _, a := range cmuxArgs {
			parts = append(parts, shellQuote(a))
		}
		remoteCmd := strings.Join(parts, " ")
		cmd := exec.CommandContext(ctx, "ssh",
			"-o", "ConnectTimeout=5",
			"-o", "StrictHostKeyChecking=accept-new",
			cfg.SSH, remoteCmd,
		)
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return "", fmt.Errorf("cmux %s on %s failed: %s", args[0], cfg.SSH, string(ee.Stderr))
			}
			return "", fmt.Errorf("cmux %s on %s failed: %w", args[0], cfg.SSH, err)
		}
		return strings.TrimSpace(string(out)), nil
	}

	// Local
	cmd := exec.CommandContext(ctx, cmuxPath, cmuxArgs...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("cmux %s failed: %s", args[0], string(ee.Stderr))
		}
		return "", fmt.Errorf("cmux %s failed: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// --- Fan-out ---

func execAllHosts(fn func(host string) (any, error)) map[string]any {
	results := make(map[string]any)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			val, err := fn(h)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				results[h] = map[string]string{"error": err.Error()}
			} else {
				results[h] = val
			}
		}(name)
	}
	wg.Wait()
	return results
}

// --- Identity ---

var wsRefRe = regexp.MustCompile(`"workspace_ref"\s*:\s*"(workspace:\d+)"`)

func getMyWorkspaceRef() string {
	myWorkspaceMu.Lock()
	defer myWorkspaceMu.Unlock()
	if myWorkspace != "" {
		return myWorkspace
	}
	out, err := cmuxExec([]string{"identify"}, "")
	if err == nil {
		if m := wsRefRe.FindStringSubmatch(out); len(m) > 1 {
			myWorkspace = m[1]
			return myWorkspace
		}
	}
	if v := os.Getenv("CMUX_WORKSPACE_ID"); v != "" {
		myWorkspace = v
		return v
	}
	return "unknown"
}

// --- Helpers ---

func textResult(s string) *mcp.CallToolResult {
	return mcp.NewToolResultText(s)
}

func jsonResult(v any) *mcp.CallToolResult {
	b, _ := json.MarshalIndent(v, "", "  ")
	return mcp.NewToolResultText(string(b))
}

func optString(req mcp.CallToolRequest, key string) string {
	args := req.GetArguments()
	v, _ := args[key].(string)
	return v
}

func optBool(req mcp.CallToolRequest, key string) bool {
	args := req.GetArguments()
	v, _ := args[key].(bool)
	return v
}

func optInt(req mcp.CallToolRequest, key string) int {
	args := req.GetArguments()
	v, _ := args[key].(float64)
	return int(v)
}

// --- Main ---

func main() {
	loadHosts()

	s := server.NewMCPServer(
		"cmux-mcp",
		"0.4.0",
		server.WithToolCapabilities(false),
	)

	// list_hosts
	s.AddTool(
		mcp.NewTool("list_hosts",
			mcp.WithDescription("List configured cmux hosts"),
		),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			type hostInfo struct {
				Name string  `json:"name"`
				Type string  `json:"type"`
				SSH  *string `json:"ssh"`
			}
			var out []hostInfo
			for name, cfg := range hosts {
				t := "local"
				var ssh *string
				if cfg.SSH != "" {
					t = "remote"
					s := cfg.SSH
					ssh = &s
				}
				out = append(out, hostInfo{Name: name, Type: t, SSH: ssh})
			}
			return jsonResult(out), nil
		},
	)

	// list_sessions
	s.AddTool(
		mcp.NewTool("list_sessions",
			mcp.WithDescription("List all cmux workspaces across all windows with their surfaces, refs, and active task titles. Omit host to list across all hosts."),
			mcp.WithString("host",
				mcp.Description("Host name. Omit to list all hosts."),
			),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			host := optString(req, "host")
			if host != "" {
				out, err := cmuxExec([]string{"tree", "--all"}, host)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return textResult(out), nil
			}
			results := execAllHosts(func(h string) (any, error) {
				return cmuxExec([]string{"tree", "--all"}, h)
			})
			return jsonResult(results), nil
		},
	)

	// session_tree
	s.AddTool(
		mcp.NewTool("session_tree",
			mcp.WithDescription("Show the pane/surface tree for a workspace"),
			mcp.WithString("host", mcp.Description("Host name. Defaults to 'local'.")),
			mcp.WithString("workspace", mcp.Description("Workspace ref (e.g. workspace:1)")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			host := optString(req, "host")
			ws := optString(req, "workspace")
			args := []string{"tree"}
			if ws != "" {
				args = append(args, "--workspace", ws)
			}
			out, err := cmuxExec(args, host)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return textResult(out), nil
		},
	)

	// spawn_claude
	s.AddTool(
		mcp.NewTool("spawn_claude",
			mcp.WithDescription(`Spawn a Claude Code session. Choose where it goes:
- "here": run claude in an existing surface (no split, no new workspace)
- "tab": new terminal tab in the same pane
- "split": split the current pane (default)
- "workspace": new workspace`),
			mcp.WithString("cwd", mcp.Required(), mcp.Description("Working directory for the Claude Code session")),
			mcp.WithString("mode", mcp.Description("Where to spawn: here, tab, split (default), workspace"), mcp.Enum("here", "tab", "split", "workspace")),
			mcp.WithString("workspace", mcp.Description("Target workspace ref. Defaults to current.")),
			mcp.WithString("surface", mcp.Description("Target surface ref (for 'here' mode).")),
			mcp.WithString("name", mcp.Description("Name for new workspace (workspace mode only).")),
			mcp.WithString("direction", mcp.Description("Split direction (split mode only)."), mcp.Enum("left", "right", "up", "down")),
			mcp.WithString("prompt", mcp.Description("Initial prompt to send after Claude starts.")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			cwd, err := req.RequireString("cwd")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			mode := optString(req, "mode")
			if mode == "" {
				mode = "split"
			}
			ws := optString(req, "workspace")
			surf := optString(req, "surface")
			name := optString(req, "name")
			direction := optString(req, "direction")
			if direction == "" {
				direction = "right"
			}
			prompt := optString(req, "prompt")

			surfRe := regexp.MustCompile(`surface:\d+`)
			wsRe := regexp.MustCompile(`workspace:\d+`)
			var msgs []string

			switch mode {
			case "here":
				cmuxExec([]string{"send", "--workspace", ws, "cd " + cwd + " && claude"}, "")
				cmuxExec([]string{"send-key", "--workspace", ws, "Enter"}, "")
				msgs = append(msgs, fmt.Sprintf("Launched claude in %s", coalesce(surf, ws, "current surface")))

			case "tab":
				args := []string{"new-surface", "--type", "terminal"}
				if ws != "" {
					args = append(args, "--workspace", ws)
				}
				out, err := cmuxExec(args, "")
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				msgs = append(msgs, "New tab: "+out)
				if m := surfRe.FindString(out); m != "" {
					surf = m
				}
				if m := wsRe.FindString(out); m != "" {
					ws = m
				}
				if surf != "" {
					sendArgs := []string{"send"}
					if ws != "" {
						sendArgs = append(sendArgs, "--workspace", ws)
					}
					sendArgs = append(sendArgs, "--surface", surf, "cd "+cwd+" && claude")
					cmuxExec(sendArgs, "")
					keyArgs := []string{"send-key"}
					if ws != "" {
						keyArgs = append(keyArgs, "--workspace", ws)
					}
					keyArgs = append(keyArgs, "--surface", surf, "Enter")
					cmuxExec(keyArgs, "")
				}

			case "split":
				args := []string{"new-split", direction}
				if ws != "" {
					args = append(args, "--workspace", ws)
				}
				out, err := cmuxExec(args, "")
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				msgs = append(msgs, "Split: "+out)
				if m := surfRe.FindString(out); m != "" {
					surf = m
				}
				if m := wsRe.FindString(out); m != "" {
					ws = m
				}
				if surf != "" {
					sendArgs := []string{"send"}
					if ws != "" {
						sendArgs = append(sendArgs, "--workspace", ws)
					}
					sendArgs = append(sendArgs, "--surface", surf, "cd "+cwd+" && claude")
					cmuxExec(sendArgs, "")
					keyArgs := []string{"send-key"}
					if ws != "" {
						keyArgs = append(keyArgs, "--workspace", ws)
					}
					keyArgs = append(keyArgs, "--surface", surf, "Enter")
					cmuxExec(keyArgs, "")
				}

			case "workspace":
				out, err := cmuxExec([]string{"new-workspace", "--cwd", cwd, "--command", "claude"}, "")
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				msgs = append(msgs, out)
				if m := wsRe.FindString(out); m != "" {
					ws = m
				}
				if name != "" && ws != "" {
					cmuxExec([]string{"rename-workspace", "--workspace", ws, name}, "")
					msgs = append(msgs, fmt.Sprintf("Renamed to %q", name))
				}
			}

			if prompt != "" && ws != "" {
				time.Sleep(5 * time.Second)
				sendArgs := []string{"send"}
				if ws != "" {
					sendArgs = append(sendArgs, "--workspace", ws)
				}
				if surf != "" {
					sendArgs = append(sendArgs, "--surface", surf)
				}
				sendArgs = append(sendArgs, prompt)
				cmuxExec(sendArgs, "")
				keyArgs := []string{"send-key"}
				if ws != "" {
					keyArgs = append(keyArgs, "--workspace", ws)
				}
				if surf != "" {
					keyArgs = append(keyArgs, "--surface", surf)
				}
				keyArgs = append(keyArgs, "Enter")
				cmuxExec(keyArgs, "")
				msgs = append(msgs, fmt.Sprintf("Sent prompt to %s", coalesce(surf, ws)))
			}

			return textResult(strings.Join(msgs, "\n")), nil
		},
	)

	// send_to_session
	s.AddTool(
		mcp.NewTool("send_to_session",
			mcp.WithDescription("Send a message to another agent's cmux workspace. Automatically prefixes with sender identity so the receiver knows who sent it and where to reply."),
			mcp.WithString("to", mcp.Required(), mcp.Description("Target workspace ref to send the message to")),
			mcp.WithString("host", mcp.Description("Host name. Defaults to 'local'.")),
			mcp.WithString("surface", mcp.Description("Surface ref if targeting a specific surface")),
			mcp.WithString("text", mcp.Required(), mcp.Description("Message to send")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			to, err := req.RequireString("to")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			text, err := req.RequireString("text")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			host := optString(req, "host")
			surf := optString(req, "surface")

			from := getMyWorkspaceRef()
			message := fmt.Sprintf("[from %s] %s", from, text)

			args := []string{"send", "--workspace", to}
			if surf != "" {
				args = append(args, "--surface", surf)
			}
			args = append(args, message)
			if _, err := cmuxExec(args, host); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			keyArgs := []string{"send-key", "--workspace", to}
			if surf != "" {
				keyArgs = append(keyArgs, "--surface", surf)
			}
			keyArgs = append(keyArgs, "Enter")
			cmuxExec(keyArgs, host)

			return textResult(fmt.Sprintf("Sent to %s.", to)), nil
		},
	)

	// send_key
	s.AddTool(
		mcp.NewTool("send_key",
			mcp.WithDescription("Send a key press to a cmux surface (e.g. Enter, Escape, ctrl-c)"),
			mcp.WithString("host", mcp.Description("Host name. Defaults to 'local'.")),
			mcp.WithString("workspace", mcp.Required(), mcp.Description("Workspace ref")),
			mcp.WithString("surface", mcp.Description("Surface ref")),
			mcp.WithString("key", mcp.Required(), mcp.Description("Key to send")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ws, err := req.RequireString("workspace")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			key, err := req.RequireString("key")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			host := optString(req, "host")
			surf := optString(req, "surface")

			args := []string{"send-key", "--workspace", ws}
			if surf != "" {
				args = append(args, "--surface", surf)
			}
			args = append(args, key)
			if _, err := cmuxExec(args, host); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return textResult("Sent key: " + key), nil
		},
	)

	// read_screen
	s.AddTool(
		mcp.NewTool("read_screen",
			mcp.WithDescription("Read terminal screen content from a cmux surface"),
			mcp.WithString("host", mcp.Description("Host name. Defaults to 'local'.")),
			mcp.WithString("workspace", mcp.Required(), mcp.Description("Workspace ref")),
			mcp.WithString("surface", mcp.Description("Surface ref")),
			mcp.WithBoolean("scrollback", mcp.Description("Include scrollback buffer")),
			mcp.WithNumber("lines", mcp.Description("Number of lines to read")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ws, err := req.RequireString("workspace")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			host := optString(req, "host")
			surf := optString(req, "surface")
			scrollback := optBool(req, "scrollback")
			lines := optInt(req, "lines")

			args := []string{"read-screen", "--workspace", ws}
			if surf != "" {
				args = append(args, "--surface", surf)
			}
			if scrollback {
				args = append(args, "--scrollback")
			}
			if lines > 0 {
				args = append(args, "--lines", fmt.Sprintf("%d", lines))
			}
			out, err := cmuxExec(args, host)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return textResult(out), nil
		},
	)

	// find_session
	s.AddTool(
		mcp.NewTool("find_session",
			mcp.WithDescription("Search cmux workspaces by name or content. Omit host to search all hosts."),
			mcp.WithString("host", mcp.Description("Host name. Omit to search all.")),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query")),
			mcp.WithBoolean("search_content", mcp.Description("Search terminal content too")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			query, err := req.RequireString("query")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			host := optString(req, "host")
			searchContent := optBool(req, "search_content")

			findArgs := func(h string) []string {
				args := []string{"find-window"}
				if searchContent {
					args = append(args, "--content")
				}
				args = append(args, query)
				return args
			}

			if host != "" {
				out, err := cmuxExec(findArgs(host), host)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return textResult(out), nil
			}
			results := execAllHosts(func(h string) (any, error) {
				return cmuxExec(findArgs(h), h)
			})
			return jsonResult(results), nil
		},
	)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}

func coalesce(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
