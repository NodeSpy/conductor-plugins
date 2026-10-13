package githubkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// isDiffTooLarge reports whether err is GitHub refusing a pull request's
// whole diff for size: a 406 on the diff media type ("the diff exceeded the
// maximum number of lines (20000)", or of files (300)).
func isDiffTooLarge(err error) bool {
	var pe *plugin.Error
	if !errors.As(err, &pe) {
		return false
	}
	status, _ := pe.Data["status"].(int)
	return status == http.StatusNotAcceptable
}

// maxPatchFiles is the most files GitHub's pull request files endpoint
// returns; past it the rebuilt diff is missing the rest.
const maxPatchFiles = 3000

// diffFromFilePatches rebuilds a pull request's unified diff from the
// per-file patches GitHub serves at /pulls/N/files when it refuses the
// whole-PR diff for size. Each file gets git-style headers around GitHub's
// patch hunks; a file GitHub sends no patch for (binary, or too large on its
// own) gets its headers and a note instead of hunks.
func (c *Client) diffFromFilePatches(ctx context.Context, tok, base, repo string, number int) (string, error) {
	type filePatch struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename"`
		Status           string `json:"status"`
		Patch            string `json:"patch"`
	}
	var files []filePatch
	err := c.listAll(ctx, tok, true, 100, func(page int) string {
		return fmt.Sprintf("%s/repos/%s/pulls/%d/files?per_page=100&page=%d", base, repo, number, page)
	}, func(b []byte) (int, error) {
		var raw []filePatch
		if err := json.Unmarshal(b, &raw); err != nil {
			return 0, err
		}
		files = append(files, raw...)
		return len(raw), nil
	})
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	omitted := 0
	for _, f := range files {
		from, to := f.Filename, f.Filename
		if f.PreviousFilename != "" {
			from = f.PreviousFilename
		}
		fmt.Fprintf(&sb, "diff --git a/%s b/%s\n", from, to)
		switch f.Status {
		case "added":
			sb.WriteString("new file mode 100644\n")
		case "removed":
			sb.WriteString("deleted file mode 100644\n")
		case "renamed":
			fmt.Fprintf(&sb, "rename from %s\nrename to %s\n", from, to)
		}
		if f.Patch == "" {
			omitted++
			sb.WriteString("# GitHub sent no patch for this file (binary, or too large on its own)\n")
			continue
		}
		oldPath, newPath := "a/"+from, "b/"+to
		if f.Status == "added" {
			oldPath = "/dev/null"
		}
		if f.Status == "removed" {
			newPath = "/dev/null"
		}
		fmt.Fprintf(&sb, "--- %s\n+++ %s\n", oldPath, newPath)
		sb.WriteString(f.Patch)
		if !strings.HasSuffix(f.Patch, "\n") {
			sb.WriteString("\n")
		}
	}
	note := ""
	if len(files) >= maxPatchFiles {
		note = fmt.Sprintf("; GitHub lists at most %d files, so later files are missing", maxPatchFiles)
	}
	log.Printf("github: pr_diff %s#%d: GitHub refused the whole diff as too large; rebuilt it from %d per-file patches (%d without a patch)%s",
		repo, number, len(files), omitted, note)
	return sb.String(), nil
}
