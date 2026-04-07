package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"math"
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

// --- Tags (in-memory + file-backed) ---

var (
	tags   map[string]map[string]bool // tag -> set of workspace refs
	tagsMu sync.RWMutex
)

const tagsFile = "/tmp/cmux-mcp-tags.json"

func loadTags() {
	tags = make(map[string]map[string]bool)
	data, err := os.ReadFile(tagsFile)
	if err != nil {
		return
	}
	var stored map[string][]string
	if err := json.Unmarshal(data, &stored); err != nil {
		return
	}
	for tag, refs := range stored {
		tags[tag] = make(map[string]bool)
		for _, ref := range refs {
			tags[tag][ref] = true
		}
	}
}

func saveTags() {
	stored := make(map[string][]string)
	for tag, refs := range tags {
		for ref := range refs {
			stored[tag] = append(stored[tag], ref)
		}
	}
	data, _ := json.MarshalIndent(stored, "", "  ")
	os.WriteFile(tagsFile, data, 0644)
}

func addTag(tag, wsRef string) {
	tagsMu.Lock()
	defer tagsMu.Unlock()
	if tags[tag] == nil {
		tags[tag] = make(map[string]bool)
	}
	tags[tag][wsRef] = true
	saveTags()
}

func removeTag(tag, wsRef string) {
	tagsMu.Lock()
	defer tagsMu.Unlock()
	if tags[tag] != nil {
		delete(tags[tag], wsRef)
		if len(tags[tag]) == 0 {
			delete(tags, tag)
		}
	}
	saveTags()
}

func getTaggedWorkspaces(tag string) []string {
	tagsMu.RLock()
	defer tagsMu.RUnlock()
	var refs []string
	for ref := range tags[tag] {
		refs = append(refs, ref)
	}
	return refs
}

func getAllTags() map[string][]string {
	tagsMu.RLock()
	defer tagsMu.RUnlock()
	result := make(map[string][]string)
	for tag, refs := range tags {
		for ref := range refs {
			result[tag] = append(result[tag], ref)
		}
	}
	return result
}

// --- Tree parsing ---

type parsedWorkspace struct {
	Ref      string
	Title    string
	Window   string
	Surfaces []parsedSurface
}

type parsedSurface struct {
	Ref   string
	Type  string // "terminal" or "browser"
	Title string
}

var (
	wsLineRe   = regexp.MustCompile(`workspace (workspace:\d+)\s+"([^"]*)"`)
	surfLineRe = regexp.MustCompile(`surface (surface:\d+)\s+\[(terminal|browser)\]\s+"([^"]*)"`)
	winLineRe  = regexp.MustCompile(`^window (window:\d+)`)
)

func parseTree(treeOutput string) []parsedWorkspace {
	var workspaces []parsedWorkspace
	var currentWin string
	var currentWS *parsedWorkspace

	for _, line := range strings.Split(treeOutput, "\n") {
		if m := winLineRe.FindStringSubmatch(line); len(m) > 1 {
			currentWin = m[1]
			continue
		}
		if m := wsLineRe.FindStringSubmatch(line); len(m) > 2 {
			if currentWS != nil {
				workspaces = append(workspaces, *currentWS)
			}
			currentWS = &parsedWorkspace{
				Ref:    m[1],
				Title:  m[2],
				Window: currentWin,
			}
			continue
		}
		if m := surfLineRe.FindStringSubmatch(line); len(m) > 3 && currentWS != nil {
			currentWS.Surfaces = append(currentWS.Surfaces, parsedSurface{
				Ref:   m[1],
				Type:  m[2],
				Title: m[3],
			})
		}
	}
	if currentWS != nil {
		workspaces = append(workspaces, *currentWS)
	}
	return workspaces
}

func filterWorkspaces(workspaces []parsedWorkspace, pattern, tag, window, surfaceType string) []parsedWorkspace {
	var result []parsedWorkspace

	// Get tagged refs if tag filter specified
	var taggedRefs map[string]bool
	if tag != "" {
		taggedRefs = make(map[string]bool)
		for _, ref := range getTaggedWorkspaces(tag) {
			taggedRefs[ref] = true
		}
	}

	var patternRe *regexp.Regexp
	if pattern != "" {
		patternRe, _ = regexp.Compile("(?i)" + pattern)
	}

	for _, ws := range workspaces {
		// Tag filter
		if taggedRefs != nil && !taggedRefs[ws.Ref] {
			continue
		}
		// Window filter
		if window != "" && ws.Window != window {
			continue
		}
		// Pattern filter — match on workspace title or any surface title
		if patternRe != nil {
			matched := patternRe.MatchString(ws.Title)
			if !matched {
				for _, s := range ws.Surfaces {
					if patternRe.MatchString(s.Title) {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
		}
		// Surface type filter — only include if workspace has at least one matching surface
		if surfaceType != "" {
			hasType := false
			for _, s := range ws.Surfaces {
				if s.Type == surfaceType {
					hasType = true
					break
				}
			}
			if !hasType {
				continue
			}
		}
		result = append(result, ws)
	}
	return result
}

// Send text to a workspace, targeting terminal surfaces
func sendToWorkspace(ws parsedWorkspace, text string, host string) error {
	// Find first terminal surface
	var targetSurf string
	for _, s := range ws.Surfaces {
		if s.Type == "terminal" {
			targetSurf = s.Ref
			break
		}
	}

	args := []string{"send", "--workspace", ws.Ref}
	if targetSurf != "" {
		args = append(args, "--surface", targetSurf)
	}
	args = append(args, text)
	if _, err := cmuxExec(args, host); err != nil {
		return err
	}

	keyArgs := []string{"send-key", "--workspace", ws.Ref}
	if targetSurf != "" {
		keyArgs = append(keyArgs, "--surface", targetSurf)
	}
	keyArgs = append(keyArgs, "Enter")
	cmuxExec(keyArgs, host)
	return nil
}

// Send key to a workspace, targeting terminal surfaces
func sendKeyToWorkspace(ws parsedWorkspace, key string, host string) error {
	var targetSurf string
	for _, s := range ws.Surfaces {
		if s.Type == "terminal" {
			targetSurf = s.Ref
			break
		}
	}

	args := []string{"send-key", "--workspace", ws.Ref}
	if targetSurf != "" {
		args = append(args, "--surface", targetSurf)
	}
	args = append(args, key)
	_, err := cmuxExec(args, host)
	return err
}

// --- Fleet spawning ---

type agentSpec struct {
	Name   string `json:"name"`
	Cwd    string `json:"cwd"`
	Prompt string `json:"prompt"`
}

type spawnedAgent struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Surface   string `json:"surface,omitempty"`
}

// gridDims returns (cols, rows) for N agents in a roughly square grid
func gridDims(n int) (int, int) {
	cols := int(math.Ceil(math.Sqrt(float64(n))))
	rows := int(math.Ceil(float64(n) / float64(cols)))
	return cols, rows
}

// spawnFleetGrid puts all agents in one workspace as split panes in a grid
func spawnFleetGrid(agents []agentSpec, host string) (string, []spawnedAgent, error) {
	if len(agents) == 0 {
		return "", nil, fmt.Errorf("no agents specified")
	}

	// Create workspace for the first agent
	out, err := cmuxExec([]string{"new-workspace", "--cwd", agents[0].Cwd, "--command", "claude"}, host)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create workspace: %w", err)
	}

	wsRe := regexp.MustCompile(`workspace:\d+`)
	surfRe := regexp.MustCompile(`surface:\d+`)

	wsRef := wsRe.FindString(out)
	firstSurf := surfRe.FindString(out)

	spawned := []spawnedAgent{{
		Name:      agents[0].Name,
		Workspace: wsRef,
		Surface:   firstSurf,
	}}

	if len(agents) == 1 {
		return wsRef, spawned, nil
	}

	// For N agents, build a grid:
	// First split right (N_cols-1) times to make columns
	// Then split each column down (N_rows-1) times
	cols, rows := gridDims(len(agents))

	// Track column surfaces — start with the first surface as column 0
	colSurfaces := []string{firstSurf}

	// Create additional columns by splitting right
	for c := 1; c < cols; c++ {
		splitOut, err := cmuxExec([]string{"new-split", "right", "--workspace", wsRef, "--surface", colSurfaces[c-1]}, host)
		if err != nil {
			break
		}
		splitStr := fmt.Sprintf("%v", splitOut)
		surf := surfRe.FindString(splitStr)
		colSurfaces = append(colSurfaces, surf)
	}

	// Now we have colSurfaces[0..cols-1], each is the top cell of a column
	// Split each column down to create rows
	// allSurfaces[col][row] = surface ref
	allSurfaces := make([][]string, len(colSurfaces))
	for c, colSurf := range colSurfaces {
		allSurfaces[c] = []string{colSurf}
		for r := 1; r < rows; r++ {
			splitOut, err := cmuxExec([]string{"new-split", "down", "--workspace", wsRef, "--surface", allSurfaces[c][r-1]}, host)
			if err != nil {
				break
			}
			splitStr := fmt.Sprintf("%v", splitOut)
			surf := surfRe.FindString(splitStr)
			allSurfaces[c] = append(allSurfaces[c], surf)
		}
	}

	// Map agent index to grid position and launch claude in each new surface
	// Agent 0 is already running in allSurfaces[0][0]
	agentIdx := 1
	for c := 0; c < len(allSurfaces) && agentIdx < len(agents); c++ {
		startRow := 0
		if c == 0 {
			startRow = 1 // skip first agent
		}
		for r := startRow; r < len(allSurfaces[c]) && agentIdx < len(agents); r++ {
			surf := allSurfaces[c][r]
			a := agents[agentIdx]

			// cd + launch claude
			cmuxExec([]string{"send", "--workspace", wsRef, "--surface", surf, "cd " + a.Cwd + " && claude"}, host)
			cmuxExec([]string{"send-key", "--workspace", wsRef, "--surface", surf, "Enter"}, host)

			spawned = append(spawned, spawnedAgent{
				Name:      a.Name,
				Workspace: wsRef,
				Surface:   surf,
			})
			agentIdx++
		}
	}

	return wsRef, spawned, nil
}

// spawnFleetTabs puts all agents as tabs in one pane of one workspace
func spawnFleetTabs(agents []agentSpec, host string) (string, []spawnedAgent, error) {
	if len(agents) == 0 {
		return "", nil, fmt.Errorf("no agents specified")
	}

	out, err := cmuxExec([]string{"new-workspace", "--cwd", agents[0].Cwd, "--command", "claude"}, host)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create workspace: %w", err)
	}

	wsRe := regexp.MustCompile(`workspace:\d+`)
	surfRe := regexp.MustCompile(`surface:\d+`)

	wsRef := wsRe.FindString(out)
	firstSurf := surfRe.FindString(out)

	spawned := []spawnedAgent{{Name: agents[0].Name, Workspace: wsRef, Surface: firstSurf}}

	for i := 1; i < len(agents); i++ {
		a := agents[i]
		tabOut, err := cmuxExec([]string{"new-surface", "--type", "terminal", "--workspace", wsRef}, host)
		if err != nil {
			continue
		}
		surf := surfRe.FindString(tabOut)
		if surf != "" {
			cmuxExec([]string{"send", "--workspace", wsRef, "--surface", surf, "cd " + a.Cwd + " && claude"}, host)
			cmuxExec([]string{"send-key", "--workspace", wsRef, "--surface", surf, "Enter"}, host)
		}
		spawned = append(spawned, spawnedAgent{Name: a.Name, Workspace: wsRef, Surface: surf})
	}

	return wsRef, spawned, nil
}

// spawnFleetWorkspaces puts each agent in its own workspace
func spawnFleetWorkspaces(agents []agentSpec, host string) (string, []spawnedAgent, error) {
	if len(agents) == 0 {
		return "", nil, fmt.Errorf("no agents specified")
	}

	wsRe := regexp.MustCompile(`workspace:\d+`)
	var spawned []spawnedAgent
	var firstWS string

	for _, a := range agents {
		out, err := cmuxExec([]string{"new-workspace", "--cwd", a.Cwd, "--command", "claude"}, host)
		if err != nil {
			continue
		}
		wsRef := wsRe.FindString(out)
		if firstWS == "" {
			firstWS = wsRef
		}
		if a.Name != "" && wsRef != "" {
			cmuxExec([]string{"rename-workspace", "--workspace", wsRef, a.Name}, host)
		}
		spawned = append(spawned, spawnedAgent{Name: a.Name, Workspace: wsRef})
	}

	return firstWS, spawned, nil
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
	loadTags()

	s := server.NewMCPServer(
		"cmux-mcp",
		"0.6.0",
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

	// tag_session
	s.AddTool(
		mcp.NewTool("tag_session",
			mcp.WithDescription("Add a tag to a workspace for group targeting. Tags persist across calls via /tmp/cmux-mcp-tags.json."),
			mcp.WithString("workspace", mcp.Required(), mcp.Description("Workspace ref (e.g. workspace:1)")),
			mcp.WithString("tag", mcp.Required(), mcp.Description("Tag name (e.g. research, finance, infra)")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ws, err := req.RequireString("workspace")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			tag, err := req.RequireString("tag")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			addTag(tag, ws)
			return textResult(fmt.Sprintf("Tagged %s with %q.", ws, tag)), nil
		},
	)

	// untag_session
	s.AddTool(
		mcp.NewTool("untag_session",
			mcp.WithDescription("Remove a tag from a workspace."),
			mcp.WithString("workspace", mcp.Required(), mcp.Description("Workspace ref (e.g. workspace:1)")),
			mcp.WithString("tag", mcp.Required(), mcp.Description("Tag to remove")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ws, err := req.RequireString("workspace")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			tag, err := req.RequireString("tag")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			removeTag(tag, ws)
			return textResult(fmt.Sprintf("Removed tag %q from %s.", tag, ws)), nil
		},
	)

	// list_tags
	s.AddTool(
		mcp.NewTool("list_tags",
			mcp.WithDescription("List all tags and their associated workspace refs."),
		),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			all := getAllTags()
			if len(all) == 0 {
				return textResult("No tags defined."), nil
			}
			return jsonResult(all), nil
		},
	)

	// broadcast
	s.AddTool(
		mcp.NewTool("broadcast",
			mcp.WithDescription(`Send a message to multiple workspaces at once. Filter by any combination of:
- pattern: regex matched against workspace and surface titles
- tag: only workspaces with this tag (set via tag_session)
- window: only workspaces in this window ref
- surface_type: only workspaces containing this surface type (terminal or browser)
Targets terminal surfaces by default. Automatically prefixes with sender identity.`),
			mcp.WithString("text", mcp.Required(), mcp.Description("Message to send")),
			mcp.WithString("pattern", mcp.Description("Regex pattern to match workspace/surface titles")),
			mcp.WithString("tag", mcp.Description("Only send to workspaces with this tag")),
			mcp.WithString("window", mcp.Description("Only send to workspaces in this window ref")),
			mcp.WithString("surface_type", mcp.Description("Only target workspaces with this surface type"), mcp.Enum("terminal", "browser")),
			mcp.WithString("host", mcp.Description("Host name. Defaults to 'local'.")),
			mcp.WithBoolean("exclude_self", mcp.Description("Exclude the sender's own workspace (default true)")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			text, err := req.RequireString("text")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			pattern := optString(req, "pattern")
			tag := optString(req, "tag")
			window := optString(req, "window")
			surfType := optString(req, "surface_type")
			host := optString(req, "host")

			// Default exclude_self to true
			excludeSelf := true
			args := req.GetArguments()
			if v, ok := args["exclude_self"]; ok {
				if b, ok := v.(bool); ok {
					excludeSelf = b
				}
			}

			// Must have at least one filter
			if pattern == "" && tag == "" && window == "" {
				return mcp.NewToolResultError("At least one filter required: pattern, tag, or window."), nil
			}

			treeOut, err := cmuxExec([]string{"tree", "--all"}, host)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			workspaces := parseTree(treeOut)
			filtered := filterWorkspaces(workspaces, pattern, tag, window, surfType)

			myRef := getMyWorkspaceRef()
			message := fmt.Sprintf("[from %s] %s", myRef, text)

			var sent []string
			var errors []string
			var mu sync.Mutex
			var wg sync.WaitGroup

			for _, ws := range filtered {
				if excludeSelf && ws.Ref == myRef {
					continue
				}
				wg.Add(1)
				go func(w parsedWorkspace) {
					defer wg.Done()
					if err := sendToWorkspace(w, message, host); err != nil {
						mu.Lock()
						errors = append(errors, fmt.Sprintf("%s: %s", w.Ref, err.Error()))
						mu.Unlock()
					} else {
						mu.Lock()
						sent = append(sent, fmt.Sprintf("%s (%s)", w.Ref, w.Title))
						mu.Unlock()
					}
				}(ws)
			}
			wg.Wait()

			var lines []string
			lines = append(lines, fmt.Sprintf("Broadcast to %d workspace(s):", len(sent)))
			for _, s := range sent {
				lines = append(lines, "  "+s)
			}
			if len(errors) > 0 {
				lines = append(lines, fmt.Sprintf("Errors (%d):", len(errors)))
				for _, e := range errors {
					lines = append(lines, "  "+e)
				}
			}
			return textResult(strings.Join(lines, "\n")), nil
		},
	)

	// broadcast_key
	s.AddTool(
		mcp.NewTool("broadcast_key",
			mcp.WithDescription(`Send a key press to multiple workspaces at once. Same filtering as broadcast.
Common use: send ctrl-c to stop all agents, Escape to cancel prompts.`),
			mcp.WithString("key", mcp.Required(), mcp.Description("Key to send (e.g. Enter, Escape, ctrl-c)")),
			mcp.WithString("pattern", mcp.Description("Regex pattern to match workspace/surface titles")),
			mcp.WithString("tag", mcp.Description("Only send to workspaces with this tag")),
			mcp.WithString("window", mcp.Description("Only send to workspaces in this window ref")),
			mcp.WithString("surface_type", mcp.Description("Only target workspaces with this surface type"), mcp.Enum("terminal", "browser")),
			mcp.WithString("host", mcp.Description("Host name. Defaults to 'local'.")),
			mcp.WithBoolean("exclude_self", mcp.Description("Exclude the sender's own workspace (default true)")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			key, err := req.RequireString("key")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			pattern := optString(req, "pattern")
			tag := optString(req, "tag")
			window := optString(req, "window")
			surfType := optString(req, "surface_type")
			host := optString(req, "host")

			excludeSelf := true
			args := req.GetArguments()
			if v, ok := args["exclude_self"]; ok {
				if b, ok := v.(bool); ok {
					excludeSelf = b
				}
			}

			if pattern == "" && tag == "" && window == "" {
				return mcp.NewToolResultError("At least one filter required: pattern, tag, or window."), nil
			}

			treeOut, err := cmuxExec([]string{"tree", "--all"}, host)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			workspaces := parseTree(treeOut)
			filtered := filterWorkspaces(workspaces, pattern, tag, window, surfType)

			myRef := getMyWorkspaceRef()

			var sent []string
			var errors []string
			var mu sync.Mutex
			var wg sync.WaitGroup

			for _, ws := range filtered {
				if excludeSelf && ws.Ref == myRef {
					continue
				}
				wg.Add(1)
				go func(w parsedWorkspace) {
					defer wg.Done()
					if err := sendKeyToWorkspace(w, key, host); err != nil {
						mu.Lock()
						errors = append(errors, fmt.Sprintf("%s: %s", w.Ref, err.Error()))
						mu.Unlock()
					} else {
						mu.Lock()
						sent = append(sent, fmt.Sprintf("%s (%s)", w.Ref, w.Title))
						mu.Unlock()
					}
				}(ws)
			}
			wg.Wait()

			var lines []string
			lines = append(lines, fmt.Sprintf("Sent key %q to %d workspace(s):", key, len(sent)))
			for _, s := range sent {
				lines = append(lines, "  "+s)
			}
			if len(errors) > 0 {
				lines = append(lines, fmt.Sprintf("Errors (%d):", len(errors)))
				for _, e := range errors {
					lines = append(lines, "  "+e)
				}
			}
			return textResult(strings.Join(lines, "\n")), nil
		},
	)

	// spawn_fleet
	s.AddTool(
		mcp.NewTool("spawn_fleet",
			mcp.WithDescription(`Spawn multiple Claude Code agents in one call with automatic tiling.

Layout modes:
- "grid": all agents in one workspace, split into an NxM grid (best for 2-6 agents)
- "tabs": all agents as tabs in one pane (best for many agents you switch between)
- "workspaces": each agent gets its own workspace/sidebar tab (best for independent work)

Each agent in the array needs at minimum a "cwd". Optionally set "name" and "prompt".
All agents are auto-tagged with the fleet_tag for easy broadcast targeting.`),
			mcp.WithString("layout", mcp.Required(), mcp.Description("Tiling layout: grid, tabs, or workspaces"), mcp.Enum("grid", "tabs", "workspaces")),
			mcp.WithString("agents_json", mcp.Required(), mcp.Description(`JSON array of agent specs. Each: {"name": "...", "cwd": "/path", "prompt": "optional initial prompt"}`)),
			mcp.WithString("fleet_tag", mcp.Description("Tag to apply to all spawned workspaces for broadcast targeting. Defaults to auto-generated.")),
			mcp.WithString("host", mcp.Description("Host name. Defaults to 'local'.")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			layout, err := req.RequireString("layout")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			agentsJSON, err := req.RequireString("agents_json")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			fleetTag := optString(req, "fleet_tag")
			host := optString(req, "host")

			var agents []agentSpec
			if err := json.Unmarshal([]byte(agentsJSON), &agents); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Invalid agents_json: %v", err)), nil
			}
			if len(agents) == 0 {
				return mcp.NewToolResultError("agents_json must contain at least one agent"), nil
			}

			// Default names
			for i := range agents {
				if agents[i].Name == "" {
					agents[i].Name = fmt.Sprintf("agent-%d", i+1)
				}
				if agents[i].Cwd == "" {
					agents[i].Cwd = "~"
				}
			}

			// Generate fleet tag if not provided
			if fleetTag == "" {
				fleetTag = fmt.Sprintf("fleet-%d", time.Now().Unix())
			}

			var wsRef string
			var spawned []spawnedAgent

			switch layout {
			case "grid":
				wsRef, spawned, err = spawnFleetGrid(agents, host)
			case "tabs":
				wsRef, spawned, err = spawnFleetTabs(agents, host)
			case "workspaces":
				wsRef, spawned, err = spawnFleetWorkspaces(agents, host)
			default:
				return mcp.NewToolResultError("Unknown layout: " + layout), nil
			}
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			// Rename workspace for grid/tabs (single workspace layouts)
			if layout == "grid" || layout == "tabs" {
				if wsRef != "" {
					cmuxExec([]string{"rename-workspace", "--workspace", wsRef, fleetTag}, host)
				}
			}

			// Tag all spawned workspaces
			taggedRefs := make(map[string]bool)
			for _, s := range spawned {
				if s.Workspace != "" && !taggedRefs[s.Workspace] {
					addTag(fleetTag, s.Workspace)
					taggedRefs[s.Workspace] = true
				}
			}

			// Send prompts (wait for claude to start)
			hasPrompts := false
			for _, a := range agents {
				if a.Prompt != "" {
					hasPrompts = true
					break
				}
			}
			if hasPrompts {
				time.Sleep(5 * time.Second)
				for i, a := range agents {
					if a.Prompt == "" || i >= len(spawned) {
						continue
					}
					s := spawned[i]
					sendArgs := []string{"send", "--workspace", s.Workspace}
					if s.Surface != "" {
						sendArgs = append(sendArgs, "--surface", s.Surface)
					}
					sendArgs = append(sendArgs, a.Prompt)
					cmuxExec(sendArgs, host)
					keyArgs := []string{"send-key", "--workspace", s.Workspace}
					if s.Surface != "" {
						keyArgs = append(keyArgs, "--surface", s.Surface)
					}
					keyArgs = append(keyArgs, "Enter")
					cmuxExec(keyArgs, host)
				}
			}

			result := map[string]any{
				"fleet_tag": fleetTag,
				"layout":    layout,
				"count":     len(spawned),
				"agents":    spawned,
			}
			return jsonResult(result), nil
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
