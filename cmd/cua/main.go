// Command cua is the computer-use automation system's CLI.
//
//	cua discover      run the model-driven agent on a goal and record an artifact
//	cua replay        run a saved artifact deterministically, with no model
//	cua approve       mark an artifact as reviewed and approved for unattended use
//	cua describe      print an artifact as a human-readable review sheet
//	cua capabilities  list saved artifacts as agent-callable tool definitions
//	cua operator      act as the human operator from a terminal (the console's API)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/NightWatchEng/rote/internal/agent"
	"github.com/NightWatchEng/rote/internal/capability"
	"github.com/NightWatchEng/rote/internal/control"
	"github.com/NightWatchEng/rote/internal/evidence"
	"github.com/NightWatchEng/rote/internal/llm"
	"github.com/NightWatchEng/rote/internal/operator"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/replay"
	"github.com/NightWatchEng/rote/internal/runner"
	"github.com/NightWatchEng/rote/internal/surface/web"
)

// Exit codes. A business outcome is not an error, but a shell caller still
// needs to tell it apart from success.
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	exitOutcome = 3
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	loadDotEnv(".env")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var code int
	switch os.Args[1] {
	case "discover":
		code = discover(ctx, os.Args[2:])
	case "replay":
		code = replayCmd(ctx, os.Args[2:])
	case "approve":
		code = approve(os.Args[2:])
	case "describe":
		code = describe(os.Args[2:])
	case "capabilities":
		code = capabilities(os.Args[2:])
	case "operator":
		code = operatorCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		code = exitUsage
	}
	stop()
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: cua <command> [flags]

  discover      --goal "sentence" | --goal FILE  --tenant FILE [--input k=v ...] [--output name:type ...] [--operator ADDR]
  replay        --artifact FILE --tenant FILE [--input k=v ...] [--operator ADDR] [--authorize-irreversible]
  approve       --artifact FILE --by NAME
  describe      --artifact FILE
  capabilities  [--dir artifacts]
  operator      wait | claim | click | type | key | resolve   [--addr ADDR] [--as NAME]

Run "cua <command> -h" for all flags.
`)
}

// inputs collects repeated --input name=value flags.
type inputs map[string]string

func (i inputs) String() string { return "" }
func (i inputs) Set(v string) error {
	name, value, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("want name=value, got %q", v)
	}
	i[name] = value
	return nil
}

// common flags shared by the two commands that drive a live session.
type sessionFlags struct {
	tenant, profiles, runDir, operator string
	headful, quiet                     bool
	operatorTimeout                    time.Duration
	in                                 inputs
}

func (f *sessionFlags) register(fs *flag.FlagSet) {
	f.in = inputs{}
	fs.StringVar(&f.tenant, "tenant", "", "tenant binding file (required)")
	fs.StringVar(&f.profiles, "profiles", "", "directory of app profiles (default: ../profiles next to the tenant file)")
	fs.StringVar(&f.runDir, "run-dir", "", "where to write evidence (default: runs/<run id>)")
	fs.StringVar(&f.operator, "operator", "", "address for the operator console, e.g. 127.0.0.1:8090; without it no human can be brought in")
	fs.DurationVar(&f.operatorTimeout, "operator-timeout", 10*time.Minute, "how long to wait for an operator to resolve an intervention")
	fs.BoolVar(&f.headful, "headful", false, "show the browser window")
	fs.BoolVar(&f.quiet, "quiet", false, "do not echo the event log to stderr")
	fs.Var(f.in, "input", "input as name=value (repeatable)")
}

// open builds everything a run needs around one live session.
func (f *sessionFlags) open(kind string) (*runner.Kit, func(), error) {
	if f.tenant == "" {
		return nil, nil, fmt.Errorf("--tenant is required")
	}
	env, err := capability.LoadTenant(f.tenant, f.profiles)
	if err != nil {
		return nil, nil, err
	}
	red, err := env.NewRedactor()
	if err != nil {
		return nil, nil, err
	}
	var echo io.Writer
	if !f.quiet {
		echo = os.Stderr
	}
	run, err := evidence.Start(f.runDir, kind, red, echo)
	if err != nil {
		return nil, nil, err
	}
	guard := policy.NewGuard(&env.Policy, func(event string, data any) { run.Log("policy", event, "", data) })
	surf, err := web.New(web.Options{Headless: !f.headful, Gate: guard.Request})
	if err != nil {
		run.Close()
		return nil, nil, err
	}
	sess := control.NewSession(surf, run)
	sess.OnTransfer = func(st control.State) { guard.HumanInControl(st.Holder == control.HolderHuman) }

	kit := &runner.Kit{Env: env, Session: sess, Guard: guard, Run: run, Redactor: red}
	cleanup := func() { surf.Close(); run.Close() }

	if f.operator != "" {
		hub := control.NewHub(sess, f.operatorTimeout)
		console, err := operator.Listen(f.operator, hub)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("operator console: %w", err)
		}
		hub.Notify = func(iv *control.Intervention) {
			fmt.Fprintf(os.Stderr, "\n>>> HUMAN NEEDED (%s): %s\n>>> open the operator console: %s\n\n", iv.Request.Kind, iv.Request.Reason, console.URL())
		}
		kit.Escalator = hub
		prev := cleanup
		cleanup = func() { console.Close(); prev() }
		fmt.Fprintf(os.Stderr, "operator console listening on %s\n", console.URL())
	}
	return kit, cleanup, nil
}

// list collects a repeatable string flag.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func discover(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	var sf sessionFlags
	sf.register(fs)
	goal := fs.String("goal", "", "the goal in plain language, or the path of a capability request file (required)")
	id := fs.String("id", "", "capability id for a plain-language goal, e.g. member.get_savings_balance (default: derived from the goal)")
	var outputs, secrets, patterns list
	fs.Var(&patterns, "input-pattern", "for a plain-language goal: name=regex an input must match, e.g. member_id=^[0-9]{1,10}$; repeatable")
	fs.Var(&outputs, "output", "for a plain-language goal: an output to extract, as name, name:type (string|integer|money) or name:type:sensitive; repeatable")
	fs.Var(&secrets, "secret", "for a plain-language goal: a secret the flow may use, by the name the tenant binds (default: all of the tenant's); repeatable")
	entry := fs.String("entry", "/", "for a plain-language goal: the entry path in the application")
	out := fs.String("out", "", "where to write the artifact (default: artifacts/<id>.json)")
	backend := fs.String("backend", "auto", "model backend: anthropic | claude-cli | auto (API if ANTHROPIC_API_KEY is set, else the claude CLI)")
	model := fs.String("model", "", "model id (default "+llm.DefaultModel+")")
	maxTurns := fs.Int("max-turns", 30, "stop after this many model turns")
	timeout := fs.Duration("timeout", 10*time.Minute, "stop after this long")
	fs.Parse(args)

	if *goal == "" {
		return fail("--goal is required: a sentence, or a request file under goals/")
	}
	client, err := pickBackend(*backend, *model)
	if err != nil {
		return fail("%v", err)
	}
	kit, cleanup, err := sf.open("discover")
	if err != nil {
		return fail("%v", err)
	}
	defer cleanup()

	var spec agent.Spec
	if raw, readErr := os.ReadFile(*goal); readErr == nil {
		if err := json.Unmarshal(raw, &spec); err != nil {
			return fail("reading goal file: %v", err)
		}
	} else {
		// The goal is a sentence. The contract comes from the flags, and any
		// input value quoted in the sentence becomes its placeholder so the
		// model plans against the parameter, not against one member.
		spec, err = inlineSpec(*goal, *id, *entry, sf.in, patterns, outputs, secrets, kit.Env)
		if err != nil {
			return fail("%v", err)
		}
	}
	if *out == "" {
		*out = filepath.Join("artifacts", spec.ID+".json")
	}

	ag := &agent.Agent{Kit: kit, LLM: client, Opts: agent.Options{MaxTurns: *maxTurns, Timeout: *timeout}}
	res := ag.Discover(ctx, &spec, sf.in, *out)
	printJSON(res)
	if res.Status != "success" {
		return exitFailed
	}
	return exitOK
}

// inlineSpec builds a capability request from a plain-language goal and the
// contract flags.
func inlineSpec(goal, id, entry string, in inputs, patterns, outputs, secrets []string, env *capability.Environment) (agent.Spec, error) {
	spec := agent.Spec{Goal: goal, Entry: entry, Version: "1.0.0"}
	pattern := map[string]string{}
	for _, p := range patterns {
		name, re, ok := strings.Cut(p, "=")
		if !ok || in[name] == "" {
			return spec, fmt.Errorf("--input-pattern %q: want name=regex for one of the --input names", p)
		}
		pattern[name] = re
	}
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return len(in[names[i]]) > len(in[names[j]]) })
	for _, name := range names {
		spec.Inputs = append(spec.Inputs, capability.Param{Name: name, Type: "string", Pattern: pattern[name], Description: "the " + strings.ReplaceAll(name, "_", " ")})
		if v := in[name]; len(v) >= 3 {
			spec.Goal = strings.ReplaceAll(spec.Goal, v, "{{inputs."+name+"}}")
		}
	}
	sort.Slice(spec.Inputs, func(i, j int) bool { return spec.Inputs[i].Name < spec.Inputs[j].Name })
	// The title and description describe the capability, not one record:
	// "Look up member <member id> and read ...".
	readable := refPattern.ReplaceAllStringFunc(spec.Goal, func(ref string) string {
		m := refPattern.FindStringSubmatch(ref)
		return "<" + strings.ReplaceAll(m[2], "_", " ") + ">"
	})
	spec.Title, spec.Description = readable, readable
	for _, o := range outputs {
		name, rest, _ := strings.Cut(o, ":")
		typ, flag, _ := strings.Cut(rest, ":")
		if typ == "" {
			typ = "string"
		}
		spec.Outputs = append(spec.Outputs, capability.Output{Name: name, Type: typ, Sensitive: flag == "sensitive",
			Description: "the " + strings.ReplaceAll(name, "_", " ")})
	}
	if len(secrets) == 0 {
		for name := range env.Tenant.Secrets {
			secrets = append(secrets, name)
		}
		sort.Strings(secrets)
	}
	spec.Secrets = secrets
	if id == "" {
		id = slug(goal)
	}
	spec.ID = id
	return spec, nil
}

var refPattern = regexp.MustCompile(`\{\{\s*(inputs|secrets)\.([a-zA-Z0-9_]+)\s*\}\}`)

// slug turns "Look up member 12345 and read..." into "look_up_member_and_read".
func slug(goal string) string {
	var b strings.Builder
	last := '_'
	for _, r := range strings.ToLower(goal) {
		if b.Len() >= 40 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			last = r
		case last != '_':
			b.WriteRune('_')
			last = '_'
		}
	}
	return strings.Trim(b.String(), "_")
}

func pickBackend(name, model string) (llm.Client, error) {
	if name == "auto" {
		name = "claude-cli"
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			name = "anthropic"
		}
	}
	switch name {
	case "anthropic":
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			return nil, fmt.Errorf("--backend anthropic needs ANTHROPIC_API_KEY (in the environment or in .env)")
		}
		return llm.NewAnthropic(model), nil
	case "claude-cli":
		return llm.NewClaudeCLI(model)
	}
	return nil, fmt.Errorf("unknown backend %q", name)
}

func replayCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	var sf sessionFlags
	sf.register(fs)
	path := fs.String("artifact", "", "artifact file (required)")
	authorize := fs.Bool("authorize-irreversible", false, "the caller consents to committing steps; honoured only for an approved artifact")
	stepTimeout := fs.Duration("step-timeout", 10*time.Second, "how long each step waits for its control and its expected screen")
	fs.Parse(args)

	if *path == "" {
		return fail("--artifact is required")
	}
	art, err := capability.Load(*path)
	if err != nil {
		return fail("%v", err)
	}
	kit, cleanup, err := sf.open("replay")
	if err != nil {
		return fail("%v", err)
	}
	defer cleanup()

	eng := &replay.Engine{Kit: kit, Opts: replay.Options{StepTimeout: *stepTimeout, AuthorizeIrreversible: *authorize}}
	res := eng.Execute(ctx, art, sf.in)
	printJSON(res)
	switch res.Status {
	case replay.StatusSuccess:
		return exitOK
	case replay.StatusOutcome:
		return exitOutcome
	}
	return exitFailed
}

func approve(args []string) int {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	path := fs.String("artifact", "", "artifact file (required)")
	by := fs.String("by", "", "who reviewed it (required)")
	fs.Parse(args)
	if *path == "" || *by == "" {
		return fail("--artifact and --by are required")
	}
	art, err := capability.Load(*path)
	if err != nil {
		return fail("%v", err)
	}
	art.Approval = capability.Approval{
		Status: capability.StatusApproved, By: *by,
		At: time.Now().UTC().Format(time.RFC3339), Digest: art.Digest(),
	}
	if err := art.Save(*path); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("approved %s@%s by %s (%s)\n", art.ID, art.Version, *by, art.Approval.Digest)
	return exitOK
}

func describe(args []string) int {
	fs := flag.NewFlagSet("describe", flag.ExitOnError)
	path := fs.String("artifact", "", "artifact file (required)")
	fs.Parse(args)
	if *path == "" {
		return fail("--artifact is required")
	}
	art, err := capability.Load(*path)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Print(capability.Describe(art))
	return exitOK
}

func capabilities(args []string) int {
	fs := flag.NewFlagSet("capabilities", flag.ExitOnError)
	dir := fs.String("dir", "artifacts", "directory of artifacts")
	fs.Parse(args)
	files, _ := filepath.Glob(filepath.Join(*dir, "*.json"))
	tools := []capability.Tool{}
	for _, f := range files {
		art, err := capability.Load(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipping %s: %v\n", f, err)
			continue
		}
		tools = append(tools, art.Tool())
	}
	printJSON(tools)
	return exitOK
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "cua: "+format+"\n", a...)
	return exitUsage
}

// loadDotEnv reads KEY=VALUE lines from a local .env file, if there is one,
// without overriding variables that are already set.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), `"'`)
		if _, set := os.LookupEnv(key); !set && value != "" {
			os.Setenv(key, value)
		}
	}
}
