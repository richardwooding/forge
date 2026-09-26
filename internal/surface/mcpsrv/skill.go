package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/forge/skills"
)

// skillRequestID names the input request forge_install_skill asks for.
const skillRequestID = "approve_skill"

// addSkillTool lets an agent install forge's Claude Code skill into whatever
// repository it is working in.
//
// It asks a human first, and the reason is not that writing a Markdown file is
// dangerous in itself. A SKILL.md is instructions that every later session in
// that repository loads automatically, without anyone opening it. Writing one
// is therefore a way to leave durable instructions for a future agent, which
// is precisely the shape of a prompt-injection foothold: text that arrives in
// one session and is obeyed in the next. The elicitation puts the path and the
// source of the skill in front of someone before that happens.
//
// Unlike forge_add_tool this is registered unconditionally, because nothing is
// compiled or executed: the content is embedded in this binary and cannot be
// chosen by the caller. What the caller chooses is only where it lands.
func (m *Manager) addSkillTool(srv *mcp.Server) {
	srv.AddTool(&mcp.Tool{
		Name: "forge_install_skill",
		Description: "Install forge's Claude Code skill into a repository, writing " +
			".claude/skills/forge/. The skill teaches an agent when to use an installed forge " +
			"tool instead of a shell pipeline, and how to write a new one. The content is " +
			"embedded in this forge binary, so it always matches the forge that is running; " +
			"the caller chooses only the destination. A human is asked to approve the path " +
			"before anything is written, because a skill is instructions that later sessions " +
			"in that repository load automatically.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"repo": {
					Type: "string",
					Description: "absolute path to the repository to install into; " +
						"omit together with user:true to install for every project",
				},
				"user": {
					Type:        "boolean",
					Description: "install under your home directory, for every project, instead of one repository",
				},
				"force": {
					Type:        "boolean",
					Description: "replace files that are already there",
				},
			},
		},
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, m.handleInstallSkill)
}

type skillArgs struct {
	Repo  string `json:"repo"`
	User  bool   `json:"user"`
	Force bool   `json:"force"`
}

// handleInstallSkill runs twice: once to work out the destination and ask, and
// once more with the answer.
func (m *Manager) handleInstallSkill(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args skillArgs
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult(fmt.Sprintf("cannot read the arguments: %v", err)), nil
		}
	}

	root, where, err := skillDestination(args)
	if err != nil {
		return errorResult(err.Error()), nil //nolint:nilerr // MCP carries tool errors as results
	}

	if resp, asked := req.Params.InputResponses[skillRequestID]; asked {
		return finishInstallSkill(root, where, args.Force, resp)
	}

	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{
			skillRequestID: &mcp.ElicitParams{
				Message: skillMessage(root, where, args.Force),
				RequestedSchema: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"approve": {
							Type:        "boolean",
							Description: "true to write the forge skill there now",
						},
					},
					Required: []string{"approve"},
				},
			},
		},
	}, nil
}

func finishInstallSkill(root, where string, force bool, resp mcp.InputResponse) (*mcp.CallToolResult, error) {
	if !approvedSkill(resp) {
		return errorResult("forge_install_skill: declined, nothing was written"), nil
	}

	res, err := skills.Install(root, force)
	if errors.Is(err, skills.ErrExists) {
		return textResult(fmt.Sprintf("the forge skill is already installed at %s; "+
			"call again with force:true to replace it", res.Root)), nil
	}
	if err != nil {
		return errorResult(err.Error()), nil //nolint:nilerr // MCP carries tool errors as results
	}

	var b strings.Builder
	fmt.Fprintf(&b, "installed the forge skill %s\n", where)
	for _, f := range res.Written {
		fmt.Fprintf(&b, "  + %s\n", filepath.Join(res.Root, f))
	}
	for _, f := range res.Skipped {
		fmt.Fprintf(&b, "  = %s (kept)\n", f)
	}
	b.WriteString("\nA new session in that repository will pick it up; " +
		"this one already has its skills loaded.")
	return textResult(b.String()), nil
}

func approvedSkill(resp mcp.InputResponse) bool {
	er, ok := resp.(*mcp.ElicitResult)
	if !ok || er.Action != "accept" {
		return false
	}
	approve, _ := er.Content["approve"].(bool)
	return approve
}

// skillDestination resolves the arguments to a directory, refusing anything
// ambiguous rather than guessing. A relative path is refused outright: the MCP
// server's working directory is not the agent's, so "." here would mean
// somewhere neither of them intended.
func skillDestination(args skillArgs) (root, where string, err error) {
	if args.User {
		if strings.TrimSpace(args.Repo) != "" {
			return "", "", errors.New("pass either repo or user:true, not both")
		}
		root, err = skills.UserDir()
		if err != nil {
			return "", "", err
		}
		return root, "for every project", nil
	}

	repo := strings.TrimSpace(args.Repo)
	if repo == "" {
		return "", "", errors.New("forge_install_skill needs an absolute path in \"repo\", " +
			"or user:true to install for every project")
	}
	if !filepath.IsAbs(repo) {
		return "", "", fmt.Errorf("%q is relative; forge's working directory is not yours, "+
			"so the path must be absolute", repo)
	}
	info, err := os.Stat(repo)
	if err != nil {
		return "", "", fmt.Errorf("cannot install into %q: %w", repo, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("%q is not a directory", repo)
	}
	return skills.ProjectDir(repo), "in " + repo, nil
}

// skillMessage is what the person actually reads. It names the exact path, so
// approving is a decision about a place rather than about a word like
// "install", and says plainly that the file becomes standing instructions.
func skillMessage(root, where string, force bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "An agent wants to install forge's Claude Code skill %s.\n\n", where)
	fmt.Fprintf(&b, "It will write:\n")
	files, err := skills.Files()
	if err == nil {
		for _, f := range files {
			fmt.Fprintf(&b, "  %s\n", filepath.Join(root, f))
		}
	}
	fmt.Fprintf(&b, "\nThe content is embedded in this forge binary and cannot be chosen by "+
		"the agent -- it teaches how to use forge tools and how to write one.\n")
	fmt.Fprintf(&b, "\nWorth knowing: a skill is instructions that every later session in that "+
		"directory loads automatically, so this leaves standing guidance rather than a one-off file.\n")
	if force {
		fmt.Fprintf(&b, "\nforce is set, so any existing files there will be replaced.\n")
	}
	return b.String()
}
