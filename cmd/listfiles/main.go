package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type fileEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	IsDir   bool   `json:"isDir"`
	ModTime string `json:"modTime"`
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	s := server.NewMCPServer("listfiles", "1.0.0",
		server.WithToolCapabilities(true),
	)

	tool := mcp.NewTool("list_files",
		mcp.WithDescription("List files and directories under a given directory"),
		mcp.WithString("path",
			mcp.Description("Directory path to list. Defaults to the server's root directory."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dir := root
		if p, ok := req.Params.Arguments.(map[string]any)["path"]; ok {
			if str, ok := p.(string); ok && str != "" {
				dir = str
			}
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to read directory %q: %v", dir, err)), nil
		}

		files := make([]fileEntry, 0, len(entries))
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to stat %q: %v", e.Name(), err)), nil
			}
			files = append(files, fileEntry{
				Name:    e.Name(),
				Size:    info.Size(),
				Mode:    info.Mode().String(),
				IsDir:   info.IsDir(),
				ModTime: info.ModTime().Format("2006-01-02 15:04:05"),
			})
		}
		sort.Slice(files, func(i, j int) bool {
			if files[i].IsDir != files[j].IsDir {
				return files[i].IsDir
			}
			return files[i].Name < files[j].Name
		})

		var sb strings.Builder
		fmt.Fprintf(&sb, "Contents of %s (%d entries):\n", dir, len(files))
		for _, f := range files {
			kind := "file"
			if f.IsDir {
				kind = "dir "
			}
			fmt.Fprintf(&sb, "  %s  %-10d %s  %s\n", kind, f.Size, f.ModTime, f.Name)
		}
		return mcp.NewToolResultText(sb.String()), nil
	})

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}
