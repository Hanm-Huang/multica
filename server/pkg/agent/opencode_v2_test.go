package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOpencodeUsesV2Contract pins which version strings switch the backend onto
// the 2.x argv. The shapes matter: extractVersionLine keeps the whole matched
// line, and 2.x reports `opencode v2.0.10` where 1.x reports a bare `1.18.31`,
// so a prefix test against "2." would miss every real 2.x runtime.
func TestOpencodeUsesV2Contract(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		version string
		builtin bool
		want    bool
	}{
		{"v2 as reported by the real CLI", "opencode v2.0.10", true, true},
		{"v2 bare semver", "2.0.10", true, true},
		{"v2 with leading v", "v2.1.0", true, true},
		{"future major", "opencode v3.0.0", true, true},
		{"v1 bare semver as reported by the real CLI", "1.18.31", true, false},
		{"v1 oldest supported", "1.1.54", true, false},
		{"unknown version fails closed onto 1.x", "", true, false},
		{"unparsable version fails closed onto 1.x", "opencode nightly", true, false},
		// A custom runtime profile wraps a binary this package did not choose,
		// so its version string does not establish the usage convention. Acting
		// on it would break working 1.x profiles on a guess.
		{"custom runtime never switches", "opencode v2.0.10", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{CLIVersion: tc.version, BuiltinRuntime: tc.builtin}
			if got := opencodeUsesV2Contract(cfg); got != tc.want {
				t.Fatalf("opencodeUsesV2Contract(%q, builtin=%v) = %v, want %v",
					tc.version, tc.builtin, got, tc.want)
			}
		})
	}
}

// TestOpencodeModelArg covers folding the thinking level into the model string,
// which is how 2.x expresses what 1.x passed as `--variant`.
func TestOpencodeModelArg(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		model         string
		thinkingLevel string
		wantModel     string
		wantOK        bool
	}{
		{"no thinking level leaves the model alone", "anthropic/claude-sonnet-4-5", "", "anthropic/claude-sonnet-4-5", true},
		{"level folds onto the model", "anthropic/claude-sonnet-4-5", "high", "anthropic/claude-sonnet-4-5#high", true},
		{"neither set", "", "", "", true},
		// 2.x resolves the default model server-side, so there is no model
		// string to attach the variant to and the level cannot be expressed.
		{"level without a model is not representable", "", "high", "", false},
		// An explicitly pinned variant wins rather than growing a second '#',
		// which OpenCode would reject.
		{"explicit variant on the model wins", "anthropic/claude-sonnet-4-5#max", "high", "anthropic/claude-sonnet-4-5#max", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotModel, gotOK := opencodeModelArg(tc.model, tc.thinkingLevel)
			if gotModel != tc.wantModel || gotOK != tc.wantOK {
				t.Fatalf("opencodeModelArg(%q, %q) = (%q, %v), want (%q, %v)",
					tc.model, tc.thinkingLevel, gotModel, gotOK, tc.wantModel, tc.wantOK)
			}
		})
	}
}

// readWorkdirConfig decodes <dir>/opencode.json for assertions.
func readWorkdirConfig(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, opencodeWorkdirConfigName))
	if err != nil {
		t.Fatalf("read workdir config: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("workdir config is not valid JSON (%s): %v", raw, err)
	}
	return doc
}

const testMCPConfig = `{"mcpServers":{"probe":{"command":"node","args":["probe.js"]}}}`

// TestOpencodeApplyWorkdirMCPConfigWritesServers checks the happy path: 2.x has
// no working env channel for MCP, so the servers have to land in the workdir's
// project config.
func TestOpencodeApplyWorkdirMCPConfigWritesServers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := opencodeApplyWorkdirMCPConfig(dir, json.RawMessage(testMCPConfig), slog.Default()); err != nil {
		t.Fatalf("apply: %v", err)
	}

	doc := readWorkdirConfig(t, dir)
	if _, ok := doc["mcp"]; !ok {
		t.Fatalf("expected an mcp key, got %v", doc)
	}
	if !strings.Contains(string(doc["mcp"]), "probe") {
		t.Fatalf("expected the probe server in %s", doc["mcp"])
	}
}

// TestOpencodeApplyWorkdirMCPConfigPreservesForeignKeys is the regression that
// guards the reason buildOpenCodeMCPConfigContent avoided this file in the first
// place: the workdir is reused across turns and the agent or the user may own
// settings in it. The daemon owns the "mcp" key and must not touch the rest.
func TestOpencodeApplyWorkdirMCPConfigPreservesForeignKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, opencodeWorkdirConfigName)
	existing := `{"model":"anthropic/claude-sonnet-4-5","permission":{"edit":"allow"}}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := opencodeApplyWorkdirMCPConfig(dir, json.RawMessage(testMCPConfig), slog.Default()); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Compare decoded values, not bytes: the rewrite re-indents the document,
	// which is expected and documented — only the content has to survive.
	doc := readWorkdirConfig(t, dir)
	if got := string(doc["model"]); got != `"anthropic/claude-sonnet-4-5"` {
		t.Fatalf("model key was not preserved, got %s", got)
	}
	var permission map[string]string
	if err := json.Unmarshal(doc["permission"], &permission); err != nil {
		t.Fatalf("permission key was not preserved as an object: %v", err)
	}
	if permission["edit"] != "allow" {
		t.Fatalf("permission key was not preserved, got %v", permission)
	}
	if _, ok := doc["mcp"]; !ok {
		t.Fatalf("expected an mcp key alongside the preserved ones, got %v", doc)
	}
}

// TestOpencodeApplyWorkdirMCPConfigClearsStaleServers covers the lifecycle: the
// workdir outlives a single turn, so dropping agent.mcp_config has to remove the
// servers the daemon previously wrote. Otherwise they keep loading forever.
func TestOpencodeApplyWorkdirMCPConfigClearsStaleServers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, opencodeWorkdirConfigName)
	if err := os.WriteFile(path, []byte(`{"model":"m","mcp":{"stale":{"type":"local","command":["x"]}}}`), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	// No mcp_config this turn.
	if err := opencodeApplyWorkdirMCPConfig(dir, nil, slog.Default()); err != nil {
		t.Fatalf("apply: %v", err)
	}

	doc := readWorkdirConfig(t, dir)
	if _, ok := doc["mcp"]; ok {
		t.Fatalf("stale mcp key survived: %v", doc)
	}
	if got := string(doc["model"]); got != `"m"` {
		t.Fatalf("clearing mcp must not disturb other keys, got %s", got)
	}
}

// TestOpencodeApplyWorkdirMCPConfigRemovesFileItCreated checks that a config
// file containing nothing but the daemon's own key is removed rather than left
// behind as an empty object in the user's workdir.
func TestOpencodeApplyWorkdirMCPConfigRemovesFileItCreated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := opencodeApplyWorkdirMCPConfig(dir, json.RawMessage(testMCPConfig), slog.Default()); err != nil {
		t.Fatalf("apply with servers: %v", err)
	}
	if err := opencodeApplyWorkdirMCPConfig(dir, nil, slog.Default()); err != nil {
		t.Fatalf("apply without servers: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, opencodeWorkdirConfigName)); !os.IsNotExist(err) {
		t.Fatalf("expected the config file to be removed, stat err = %v", err)
	}
}

// TestOpencodeApplyWorkdirMCPConfigNoopWithoutConfig makes sure a task with no
// mcp_config never creates a file in a workdir that had none.
func TestOpencodeApplyWorkdirMCPConfigNoopWithoutConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := opencodeApplyWorkdirMCPConfig(dir, nil, slog.Default()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, opencodeWorkdirConfigName)); !os.IsNotExist(err) {
		t.Fatalf("expected no config file to be created, stat err = %v", err)
	}
}

// TestOpencodeApplyWorkdirMCPConfigRefusesUnparsableFile pins the fail-loud
// path. The file belongs to the agent or the user; overwriting one the daemon
// cannot round-trip would destroy their settings silently.
func TestOpencodeApplyWorkdirMCPConfigRefusesUnparsableFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, opencodeWorkdirConfigName)
	garbage := []byte("{not json at all")
	if err := os.WriteFile(path, garbage, 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	err := opencodeApplyWorkdirMCPConfig(dir, json.RawMessage(testMCPConfig), slog.Default())
	if err == nil {
		t.Fatal("expected an error for an unparsable config file")
	}
	if !strings.Contains(err.Error(), opencodeWorkdirConfigName) {
		t.Fatalf("error should name the offending file, got %v", err)
	}

	// The user's bytes must still be there.
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}
	if string(after) != string(garbage) {
		t.Fatalf("refused write still modified the file: %s", after)
	}
}

// TestOpencodeV2ArgvOmitsDirAndFoldsVariant is the end-to-end regression for
// GH #8586: on a 2.x runtime the daemon must not pass `--dir` (the CLI rejects
// the unknown flag and the run dies before it starts) and must express the
// thinking level through the model string instead of `--variant`.
func TestOpencodeV2ArgvOmitsDirAndFoldsVariant(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	argsFile := filepath.Join(tempDir, "argv.txt")
	pwdFile := filepath.Join(tempDir, "pwd.txt")
	fakePath := filepath.Join(tempDir, "opencode")
	writeTestExecutable(t, fakePath, []byte(fakeOpencodeScript()))

	workDir := t.TempDir()

	backend, err := New("opencode", Config{
		ExecutablePath: fakePath,
		CLIVersion:     "opencode v2.0.10",
		BuiltinRuntime: true,
		Logger:         slog.Default(),
		Env: map[string]string{
			"OPENCODE_ARGS_FILE": argsFile,
			"OPENCODE_PWD_FILE":  pwdFile,
		},
	})
	if err != nil {
		t.Fatalf("new opencode backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, "prompt-ignored", ExecOptions{
		Cwd:           workDir,
		Model:         "anthropic/claude-sonnet-4-5",
		ThinkingLevel: "high",
		Timeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	<-session.Result

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")

	if containsString(args, "--dir") {
		t.Fatalf("2.x argv must not carry --dir, got %q", args)
	}
	if containsString(args, "--variant") {
		t.Fatalf("2.x argv must not carry --variant, got %q", args)
	}
	if !containsString(args, "anthropic/claude-sonnet-4-5#high") {
		t.Fatalf("expected the thinking level folded into the model string, got %q", args)
	}

	// Dropping --dir is only safe because cwd still anchors discovery, so the
	// PWD the child sees must remain the task workdir.
	gotPWD, err := os.ReadFile(pwdFile)
	if err != nil {
		t.Fatalf("read PWD file: %v", err)
	}
	if strings.TrimSpace(string(gotPWD)) != workDir {
		t.Fatalf("expected PWD %q, got %q", workDir, strings.TrimSpace(string(gotPWD)))
	}
}

// TestOpencodeV1ArgvKeepsDirAndVariant is the other half of the contract: the
// 1.x path is what every existing runtime uses and must not shift.
func TestOpencodeV1ArgvKeepsDirAndVariant(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	argsFile := filepath.Join(tempDir, "argv.txt")
	fakePath := filepath.Join(tempDir, "opencode")
	writeTestExecutable(t, fakePath, []byte(fakeOpencodeScript()))

	workDir := t.TempDir()

	backend, err := New("opencode", Config{
		ExecutablePath: fakePath,
		CLIVersion:     "1.18.31",
		BuiltinRuntime: true,
		Logger:         slog.Default(),
		Env:            map[string]string{"OPENCODE_ARGS_FILE": argsFile},
	})
	if err != nil {
		t.Fatalf("new opencode backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, "prompt-ignored", ExecOptions{
		Cwd:           workDir,
		Model:         "anthropic/claude-sonnet-4-5",
		ThinkingLevel: "high",
		Timeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	<-session.Result

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")

	if !containsString(args, "--dir") {
		t.Fatalf("1.x argv must still carry --dir, got %q", args)
	}
	if !containsString(args, "--variant") {
		t.Fatalf("1.x argv must still carry --variant, got %q", args)
	}
	if !containsString(args, "anthropic/claude-sonnet-4-5") {
		t.Fatalf("1.x model must stay unfolded, got %q", args)
	}
	if containsString(args, "anthropic/claude-sonnet-4-5#high") {
		t.Fatalf("1.x must not fold the variant into the model, got %q", args)
	}
}

// TestOpencodeV2WritesMCPConfigIntoWorkdir checks the wiring end to end: a 2.x
// run must leave the servers in the workdir's project config, because the env
// channel the 1.x path uses is inert on 2.x.
func TestOpencodeV2WritesMCPConfigIntoWorkdir(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "opencode")
	writeTestExecutable(t, fakePath, []byte(fakeOpencodeScript()))

	workDir := t.TempDir()

	backend, err := New("opencode", Config{
		ExecutablePath: fakePath,
		CLIVersion:     "opencode v2.0.10",
		BuiltinRuntime: true,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("new opencode backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, "prompt-ignored", ExecOptions{
		Cwd:       workDir,
		McpConfig: json.RawMessage(testMCPConfig),
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	<-session.Result

	doc := readWorkdirConfig(t, workDir)
	if !strings.Contains(string(doc["mcp"]), "probe") {
		t.Fatalf("expected the mcp servers in the workdir config, got %v", doc)
	}
}

// TestOpencodeSessionTracker covers the handoff the cancellation path depends
// on, including the nil receiver every direct-processEvents test relies on.
func TestOpencodeSessionTracker(t *testing.T) {
	t.Parallel()

	var nilTracker *opencodeSessionTracker
	nilTracker.set("ses_ignored") // must not panic
	if got := nilTracker.get(); got != "" {
		t.Fatalf("nil tracker should report no session, got %q", got)
	}

	tracker := &opencodeSessionTracker{}
	if got := tracker.get(); got != "" {
		t.Fatalf("fresh tracker should report no session, got %q", got)
	}
	tracker.set("")
	if got := tracker.get(); got != "" {
		t.Fatalf("empty session id must be ignored, got %q", got)
	}
	tracker.set("ses_first")
	tracker.set("ses_second")
	if got := tracker.get(); got != "ses_first" {
		t.Fatalf("tracker should keep the first session id, got %q", got)
	}
}

// TestOpencodeProcessEventsPublishesSession pins that the scanner hands the
// session id to the tracker. Without it the cancellation path has no id to
// interrupt and a cancelled 2.x run keeps going server-side.
func TestOpencodeProcessEventsPublishesSession(t *testing.T) {
	t.Parallel()

	b := &opencodeBackend{cfg: Config{Logger: slog.Default()}, session: &opencodeSessionTracker{}}
	ch := make(chan Message, 8)
	lines := `{"type":"step_start","timestamp":1,"sessionID":"ses_abc","part":{}}` + "\n" +
		`{"type":"step_finish","timestamp":2,"sessionID":"ses_abc","part":{}}` + "\n"

	go func() {
		b.processEvents(strings.NewReader(lines), ch)
		close(ch)
	}()
	for range ch {
	}

	if got := b.session.get(); got != "ses_abc" {
		t.Fatalf("expected the scanner to publish the session id, got %q", got)
	}
}

// TestOpencodeInterruptSessionWithoutSessionIsNoop makes sure the cancellation
// path does not spawn a process when no event ever carried a session id.
func TestOpencodeInterruptSessionWithoutSessionIsNoop(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	marker := filepath.Join(tempDir, "called.txt")
	fakePath := filepath.Join(tempDir, "opencode")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"))

	opencodeInterruptSession(NewCommand(fakePath, nil), "", slog.Default())

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("interrupt must not run the CLI without a session id, stat err = %v", err)
	}
}

// TestOpencodeInterruptSessionCallsAPI pins the request the daemon makes. The
// route is what actually stops a 2.x run; signalling the client's process group
// leaves the background service working.
func TestOpencodeInterruptSessionCallsAPI(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	argsFile := filepath.Join(tempDir, "argv.txt")
	fakePath := filepath.Join(tempDir, "opencode")
	script := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\" >> \"" + argsFile + "\"; done\n"
	writeTestExecutable(t, fakePath, []byte(script))

	opencodeInterruptSession(NewCommand(fakePath, nil), "ses_abc", slog.Default())

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")

	want := []string{"api", "POST", "/api/session/ses_abc/interrupt"}
	if len(args) != len(want) {
		t.Fatalf("expected argv %q, got %q", want, args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("expected argv %q, got %q", want, args)
		}
	}
}

// TestOpencodeInterruptSessionSurvivesFailure keeps the call best-effort: a
// service that is gone or answers non-zero must not stop the caller, because
// the process-group signalling behind it is what the 1.x path always relied on.
func TestOpencodeInterruptSessionSurvivesFailure(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "opencode")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\necho 'connection refused' >&2\nexit 1\n"))

	// The assertion is that this returns rather than panicking or blocking.
	opencodeInterruptSession(NewCommand(fakePath, nil), "ses_abc", slog.Default())
}
