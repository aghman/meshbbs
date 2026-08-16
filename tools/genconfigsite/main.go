// Command genconfigsite regenerates the configuration reference on the project
// site from the same struct tags that generate docs/config.md.
//
// The site has no build step: site/config-reference.html is a real file, served
// as-is, and this command rewrites the block between the GENERATED markers in
// place. Everything outside those markers — the page chrome, the prose, the
// navigation — is hand-written and is left alone.
//
// Run it whenever a config key changes:
//
//	go run ./tools/genconfigsite
package main

import (
	"bytes"
	"fmt"
	"html"
	"os"
	"sort"
	"strings"

	"github.com/aghman/meshbbs/internal/config"
)

const (
	target = "site/config-reference.html"
	begin  = "<!-- BEGIN GENERATED — go run ./tools/genconfigsite -->"
	end    = "<!-- END GENERATED -->"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "genconfigsite:", err)
		os.Exit(1)
	}
}

func run() error {
	page, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	i := bytes.Index(page, []byte(begin))
	j := bytes.Index(page, []byte(end))
	if i < 0 || j < 0 || j < i {
		return fmt.Errorf("%s: GENERATED markers not found", target)
	}

	var out bytes.Buffer
	out.Write(page[:i+len(begin)])
	out.WriteString("\n")
	writeBody(&out)
	out.Write(page[j:])

	return os.WriteFile(target, out.Bytes(), 0o644)
}

// writeBody emits one section per config table, in the order the reference
// gives them, preceded by a table of contents.
func writeBody(w *bytes.Buffer) {
	entries := config.Reference()

	var sections []string
	bySection := map[string][]config.Entry{}
	for _, en := range entries {
		s := strings.SplitN(en.Key, ".", 2)[0]
		if _, seen := bySection[s]; !seen {
			sections = append(sections, s)
		}
		bySection[s] = append(bySection[s], en)
	}
	sort.Strings(sections)

	fmt.Fprintf(w, "  <div class=\"toc\">\n    <span>Sections</span>\n")
	for _, s := range sections {
		fmt.Fprintf(w, "    <a href=\"#%s\">[%s]</a>\n", s, s)
	}
	fmt.Fprintf(w, "  </div>\n\n")

	for _, s := range sections {
		fmt.Fprintf(w, "  <section id=%q>\n", s)
		fmt.Fprintf(w, "    <div class=\"eyebrow\">Section</div>\n")
		fmt.Fprintf(w, "    <div class=\"col prose\">\n      <h2><code>[%s]</code></h2>\n", s)
		if doc := config.SectionDoc(s); doc != "" {
			fmt.Fprintf(w, "      <p>%s</p>\n", html.EscapeString(doc))
		}
		fmt.Fprintf(w, "    </div>\n\n    <div class=\"gap-s\"></div>\n\n    <div class=\"rows\">\n")

		for _, en := range bySection[s] {
			def := en.Default
			if def == "" {
				def = "(empty)"
			}
			fmt.Fprintf(w, "      <div class=\"row\">\n")
			fmt.Fprintf(w, "        <div class=\"k\"><code>%s</code>"+
				"<br><span class=\"meta\">%s · %s</span>"+
				"<br><span class=\"meta env\">%s</span></div>\n",
				html.EscapeString(en.Key),
				html.EscapeString(en.Type),
				html.EscapeString(def),
				html.EscapeString(en.Env))
			fmt.Fprintf(w, "        <div class=\"v\">%s</div>\n", html.EscapeString(en.Doc))
			fmt.Fprintf(w, "      </div>\n")
		}

		fmt.Fprintf(w, "    </div>\n  </section>\n\n")
	}
}
