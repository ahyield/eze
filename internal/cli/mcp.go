package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpInstructions = `eze hosts small web apps for an organisation: a folder of static files
becomes https://<site>.<domain>/, visible only to signed-in colleagues. Sites get a browser SDK
(window.eze) with a database, realtime updates, file uploads, multiplayer rooms, AI and the
viewer's identity. There is no server-side code.

To build a site: read sdk_docs first, write plain HTML/CSS/JS, then deploy with deploy_folder
(when you can write files to disk) or deploy_files (when you can't). Deploying replaces the
whole site, so always send every file. Use list_sites before choosing a name, so you don't
replace someone else's site by accident.`

// MCP serves eze's tools over stdio to an MCP client (Claude Code, Claude Desktop, Cursor...).
// It uses the login saved by `eze login`.
func MCP(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: eze mcp (run by your MCP client, not by hand)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return newMCPServer(newClient).Run(ctx, &mcp.StdioTransport{})
}

func boolPtr(b bool) *bool { return &b }

// newMCPServer builds the server. connect is called per tool call, so a client started
// before `eze login` works as soon as you log in.
func newMCPServer(connect func() (*client, error)) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "eze", Version: "0.1.0"}, &mcp.ServerOptions{Instructions: mcpInstructions})
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}
	replaces := &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), IdempotentHint: true, OpenWorldHint: boolPtr(false)}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "whoami",
		Description: "Show which eze server this is and who it acts as.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, whoamiOut, error) {
		c, err := connect()
		if err != nil {
			return nil, whoamiOut{}, err
		}
		var me user
		if err := c.do("GET", "/_q/api/me", nil, "", &me); err != nil {
			return nil, whoamiOut{}, err
		}
		return nil, whoamiOut{Email: me.Email, Name: me.Name, Server: c.cfg.Server}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sites",
		Description: "List every site on this eze server, most recently updated first, with its URL and who last deployed it.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listSitesOut, error) {
		c, err := connect()
		if err != nil {
			return nil, listSitesOut{}, err
		}
		var sites []site
		if err := c.do("GET", "/_q/api/sites", nil, "", &sites); err != nil {
			return nil, listSitesOut{}, err
		}
		return nil, listSitesOut{Sites: sites}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_site",
		Description: "Show one site: its URL, owner, size and recent deploy history.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in siteIn) (*mcp.CallToolResult, siteDetail, error) {
		c, err := connect()
		if err != nil {
			return nil, siteDetail{}, err
		}
		var out siteDetail
		err = c.do("GET", "/_q/api/sites/"+url.PathEscape(in.Site), nil, "", &out)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "deploy_folder",
		Description: "Deploy a folder from this computer as a site, replacing everything the site had. " +
			"Hidden files, node_modules, AGENTS.md, CLAUDE.md and eze.json are not uploaded. " +
			"The site name defaults to the folder's eze.json, then the folder name.",
		Annotations: replaces,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deployFolderIn) (*mcp.CallToolResult, deployOut, error) {
		if !filepath.IsAbs(in.Path) {
			return nil, deployOut{}, fmt.Errorf("path must be absolute, got %q", in.Path)
		}
		c, err := connect()
		if err != nil {
			return nil, deployOut{}, err
		}
		res, err := deployDir(c, in.Path, in.Site, nil)
		if err != nil {
			return nil, deployOut{}, err
		}
		return nil, deployOut{Site: res.Site.Name, URL: res.Site.URL, Files: res.Files, Warning: res.Warning}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "deploy_files",
		Description: "Deploy a site from files given inline, replacing everything the site had. " +
			"Send every file the site needs, including index.html. Use base64 for binary files such as images.",
		Annotations: replaces,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deployFilesIn) (*mcp.CallToolResult, deployOut, error) {
		if !siteNameRe.MatchString(in.Site) {
			return nil, deployOut{}, fmt.Errorf("%q is not a valid site name; use lowercase letters, digits and hyphens", in.Site)
		}
		var buf bytes.Buffer
		warning, err := packFiles(in.Files, &buf)
		if err != nil {
			return nil, deployOut{}, err
		}
		c, err := connect()
		if err != nil {
			return nil, deployOut{}, err
		}
		var res site
		if err := c.do("PUT", "/_q/api/sites/"+url.PathEscape(in.Site), &buf, "application/gzip", &res); err != nil {
			return nil, deployOut{}, err
		}
		return nil, deployOut{Site: res.Name, URL: res.URL, Files: len(in.Files), Warning: warning}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "query_collection",
		Description: "Read documents from a site's database collection, e.g. to check what a deployed site has stored. " +
			"Filters are field equality only; newest first unless order is \"asc\".",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryIn) (*mcp.CallToolResult, queryOut, error) {
		c, err := connect()
		if err != nil {
			return nil, queryOut{}, err
		}
		base, err := siteBase(c.cfg.Server, in.Site)
		if err != nil {
			return nil, queryOut{}, err
		}
		q := url.Values{"order": {"desc"}, "limit": {"50"}}
		if in.Order == "asc" {
			q.Set("order", "asc")
		}
		if in.Limit > 0 {
			q.Set("limit", strconv.Itoa(min(in.Limit, 1000)))
		}
		if len(in.Where) > 0 {
			where, _ := json.Marshal(in.Where)
			q.Set("where", string(where))
		}
		siteClient := &client{cfg: &Config{Server: base, Token: c.cfg.Token}, http: c.http}
		var docs []map[string]any
		if err := siteClient.do("GET", "/_q/db/"+url.PathEscape(in.Collection)+"?"+q.Encode(), nil, "", &docs); err != nil {
			return nil, queryOut{}, err
		}
		return nil, queryOut{Documents: docs}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "sdk_docs",
		Description: "The reference for building eze sites: the rules and the browser SDK (database, realtime, files, rooms, AI, identity). Read it before writing a site.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: sdkDocs()}}}, nil, nil
	})

	s.AddResource(&mcp.Resource{
		URI:         "eze://docs/sdk",
		Name:        "eze-sdk",
		Title:       "eze site SDK reference",
		Description: "How to build an eze site and use window.eze",
		MIMEType:    "text/markdown",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "eze://docs/sdk", MIMEType: "text/markdown", Text: sdkDocs()}}}, nil
	})
	return s
}

type whoamiOut struct {
	Email  string `json:"email"`
	Name   string `json:"name"`
	Server string `json:"server"`
}

type listSitesOut struct {
	Sites []site `json:"sites"`
}

type siteIn struct {
	Site string `json:"site" jsonschema:"the site name, e.g. lunch-vote"`
}

type siteDetail struct {
	site
	Deploys []struct {
		DeployedBy string    `json:"deployed_by"`
		Files      int       `json:"files"`
		Bytes      int64     `json:"bytes"`
		CreatedAt  time.Time `json:"created_at"`
	} `json:"deploys"`
}

type deployFolderIn struct {
	Path string `json:"path" jsonschema:"absolute path of the folder to deploy"`
	Site string `json:"site,omitempty" jsonschema:"site name; lowercase letters, digits and hyphens"`
}

type inlineFile struct {
	Path    string `json:"path" jsonschema:"path inside the site, e.g. index.html or assets/app.js"`
	Content string `json:"content" jsonschema:"the file's contents"`
	Base64  bool   `json:"base64,omitempty" jsonschema:"true if content is base64, for binary files"`
}

type deployFilesIn struct {
	Site  string       `json:"site" jsonschema:"site name; lowercase letters, digits and hyphens"`
	Files []inlineFile `json:"files" jsonschema:"every file of the site"`
}

type deployOut struct {
	Site    string `json:"site"`
	URL     string `json:"url"`
	Files   int    `json:"files"`
	Warning string `json:"warning,omitempty"`
}

type queryIn struct {
	Site       string         `json:"site" jsonschema:"the site name"`
	Collection string         `json:"collection" jsonschema:"the collection name, e.g. posts"`
	Where      map[string]any `json:"where,omitempty" jsonschema:"equality filters on fields, e.g. {\"status\": \"draft\"}"`
	Limit      int            `json:"limit,omitempty" jsonschema:"how many documents to return (default 50, max 1000)"`
	Order      string         `json:"order,omitempty" jsonschema:"desc (newest first, the default) or asc"`
}

type queryOut struct {
	Documents []map[string]any `json:"documents"`
}

// sdkDocs is the AGENTS.md that `eze init` writes, without a site name filled in.
func sdkDocs() string {
	b, _ := templatesFS.ReadFile("templates/AGENTS.md")
	return strings.ReplaceAll(string(b), "{{SITE}}", "your-site")
}

// siteBase turns the server's apex URL into a site's URL: https://eze.example.com and
// "blog" give https://blog.eze.example.com.
func siteBase(server, name string) (string, error) {
	if !siteNameRe.MatchString(name) {
		return "", fmt.Errorf("%q is not a valid site name", name)
	}
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	u.Host = name + "." + u.Host
	u.Path = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// packFiles writes inline files as a gzipped tarball, applying the same rules as `eze deploy`.
func packFiles(files []inlineFile, w *bytes.Buffer) (warning string, err error) {
	if len(files) == 0 {
		return "", errors.New("no files to deploy")
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	seen := map[string]bool{}
	hasIndex := false
	for _, f := range files {
		name := strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(f.Path, "\\", "/")), "/")
		if f.Path == "" || strings.Contains(f.Path, "..") || name == "" {
			return "", fmt.Errorf("invalid file path %q", f.Path)
		}
		if seen[name] {
			return "", fmt.Errorf("file %q given twice", name)
		}
		seen[name] = true
		hasIndex = hasIndex || name == "index.html"
		body := []byte(f.Content)
		if f.Base64 {
			if body, err = base64.StdEncoding.DecodeString(f.Content); err != nil {
				return "", fmt.Errorf("%s: content is not valid base64", name)
			}
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), ModTime: time.Now()}); err != nil {
			return "", err
		}
		if _, err := tw.Write(body); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if !hasIndex {
		warning = "no index.html, so the site root will show \"not found\""
	}
	return warning, nil
}
