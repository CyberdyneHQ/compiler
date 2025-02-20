package transform

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"

	astro "github.com/withastro/compiler/internal"
	"github.com/withastro/compiler/internal/js_scanner"
	"github.com/withastro/compiler/internal/loc"
	"golang.org/x/net/html/atom"
	a "golang.org/x/net/html/atom"
)

type TransformOptions struct {
	Scope            string
	Filename         string
	Pathname         string
	InternalURL      string
	SourceMap        string
	Site             string
	ProjectRoot      string
	PreprocessStyle  interface{}
	StaticExtraction bool
}

func Transform(doc *astro.Node, opts TransformOptions) *astro.Node {
	shouldScope := len(doc.Styles) > 0 && ScopeStyle(doc.Styles, opts)
	walk(doc, func(n *astro.Node) {
		ExtractScript(doc, n, &opts)
		AddComponentProps(doc, n, &opts)
		if shouldScope {
			ScopeElement(n, opts)
		}
	})
	NormalizeSetDirectives(doc)

	// Important! Remove scripts from original location *after* walking the doc
	for _, script := range doc.Scripts {
		script.Parent.RemoveChild(script)
	}

	// If we've emptied out all the nodes, this was a Fragment that only contained hoisted elements
	// Add an empty FrontmatterNode to allow the empty component to be printed
	if doc.FirstChild == nil {
		empty := &astro.Node{
			Type: astro.FrontmatterNode,
		}
		empty.AppendChild(&astro.Node{
			Type: astro.TextNode,
			Data: "",
		})
		doc.AppendChild(empty)
	}

	TrimTrailingSpace(doc)

	return doc
}

func ExtractStyles(doc *astro.Node) {
	walk(doc, func(n *astro.Node) {
		if n.Type == astro.ElementNode && n.DataAtom == a.Style {
			if HasSetDirective(n) || HasInlineDirective(n) {
				return
			}
			// Do not extract <style> inside of SVGs
			if n.Parent != nil && n.Parent.DataAtom == atom.Svg {
				return
			}
			// prepend node to maintain authored order
			doc.Styles = append([]*astro.Node{n}, doc.Styles...)
		}
	})
	// Important! Remove styles from original location *after* walking the doc
	for _, style := range doc.Styles {
		style.Parent.RemoveChild(style)
	}
}

func NormalizeSetDirectives(doc *astro.Node) {
	var nodes []*astro.Node
	var directives []*astro.Attribute
	walk(doc, func(n *astro.Node) {
		if n.Type == astro.ElementNode && HasSetDirective(n) {
			for _, attr := range n.Attr {
				if attr.Key == "set:html" || attr.Key == "set:text" {
					nodes = append(nodes, n)
					directives = append(directives, &attr)
					return
				}
			}
		}
	})

	if len(nodes) > 0 {
		for i, n := range nodes {
			directive := directives[i]
			n.RemoveAttribute(directive.Key)
			expr := &astro.Node{
				Type:       astro.ElementNode,
				Data:       "astro:expression",
				Expression: true,
			}
			loc := make([]loc.Loc, 1)
			loc = append(loc, directive.ValLoc)
			data := directive.Val
			if directive.Key == "set:html" {
				data = fmt.Sprintf("$$unescapeHTML(%s)", data)
			}
			expr.AppendChild(&astro.Node{
				Type: astro.TextNode,
				Data: data,
				Loc:  loc,
			})

			shouldWarn := false
			// Remove all existing children
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if !shouldWarn {
					shouldWarn = c.Type == astro.CommentNode || (c.Type == astro.TextNode && len(strings.TrimSpace(c.Data)) != 0)
				}
				n.RemoveChild(c)
			}
			if shouldWarn {
				fmt.Printf("<%s> uses the \"%s\" directive, but has child nodes which will be overwritten. Remove the child nodes to suppress this warning.\n", n.Data, directive.Key)
			}
			n.AppendChild(expr)
		}
	}
}

func TrimTrailingSpace(doc *astro.Node) {
	if doc.LastChild == nil {
		return
	}

	if doc.LastChild.Type == astro.TextNode {
		doc.LastChild.Data = strings.TrimRightFunc(doc.LastChild.Data, unicode.IsSpace)
		return
	}

	n := doc.LastChild
	for i := 0; i < 2; i++ {
		// Loop through implicit nodes to find final trailing text node (html > body > #text)
		if n != nil && n.Type == astro.ElementNode && IsImplictNode(n) {
			n = n.LastChild
			continue
		} else {
			n = nil
			break
		}
	}

	if n != nil && n.Type == astro.TextNode {
		n.Data = strings.TrimRightFunc(n.Data, unicode.IsSpace)
	}
}

// TODO: cleanup sibling whitespace after removing scripts/styles
// func removeSiblingWhitespace(n *astro.Node) {
// 	if c := n.NextSibling; c != nil && c.Type == astro.TextNode {
// 		content := strings.TrimSpace(c.Data)
// 		if len(content) == 0 {
// 			c.Parent.RemoveChild(c)
// 		}
// 	}
// }

func ExtractScript(doc *astro.Node, n *astro.Node, opts *TransformOptions) {
	if n.Type == astro.ElementNode && n.DataAtom == a.Script {
		if HasSetDirective(n) || HasInlineDirective(n) {
			return
		}

		// if <script>, hoist to the document root
		// If also using define:vars, that overrides the hoist tag.
		if (hasTruthyAttr(n, "hoist") && !HasAttr(n, "define:vars")) ||
			len(n.Attr) == 0 || (len(n.Attr) == 1 && n.Attr[0].Key == "src") {
			shouldAdd := true
			for _, attr := range n.Attr {
				if attr.Key == "hoist" {
					fmt.Printf("%s: <script hoist> is no longer needed. You may remove the `hoist` attribute.\n", opts.Filename)
				}
				if attr.Key == "src" {
					if attr.Type == astro.ExpressionAttribute {
						if opts.StaticExtraction {
							shouldAdd = false
							fmt.Printf("%s: <script> uses the expression {%s} on the src attribute and will be ignored. Use a string literal on the src attribute instead.\n", opts.Filename, attr.Val)
						}
						break
					}
				}
			}

			// prepend node to maintain authored order
			if shouldAdd {
				doc.Scripts = append([]*astro.Node{n}, doc.Scripts...)
			}
		}
	}
}

func AddComponentProps(doc *astro.Node, n *astro.Node, opts *TransformOptions) {
	if n.Type == astro.ElementNode && (n.Component || n.CustomElement) {
		for _, attr := range n.Attr {
			id := n.Data
			if n.CustomElement {
				id = fmt.Sprintf("'%s'", id)
			}

			if strings.HasPrefix(attr.Key, "client:") {
				parts := strings.Split(attr.Key, ":")
				directive := parts[1]

				// Add the hydration directive so it can be extracted statically.
				doc.HydrationDirectives[directive] = true

				hydrationAttr := astro.Attribute{
					Key: "client:component-hydration",
					Val: directive,
				}
				n.Attr = append(n.Attr, hydrationAttr)

				if attr.Key == "client:only" {
					doc.ClientOnlyComponentNodes = append([]*astro.Node{n}, doc.ClientOnlyComponentNodes...)

					match := matchNodeToImportStatement(doc, n)
					if match != nil {
						doc.ClientOnlyComponents = append(doc.ClientOnlyComponents, &astro.HydratedComponentMetadata{
							ExportName:   match.ExportName,
							Specifier:    match.Specifier,
							ResolvedPath: resolveIdForMatch(match, opts),
						})
					}

					break
				}
				// prepend node to maintain authored order
				doc.HydratedComponentNodes = append([]*astro.Node{n}, doc.HydratedComponentNodes...)
				pathAttr := astro.Attribute{
					Key:  "client:component-path",
					Val:  fmt.Sprintf("$$metadata.getPath(%s)", id),
					Type: astro.ExpressionAttribute,
				}
				n.Attr = append(n.Attr, pathAttr)

				exportAttr := astro.Attribute{
					Key:  "client:component-export",
					Val:  fmt.Sprintf("$$metadata.getExport(%s)", id),
					Type: astro.ExpressionAttribute,
				}
				n.Attr = append(n.Attr, exportAttr)

				match := matchNodeToImportStatement(doc, n)
				if match != nil {
					doc.HydratedComponents = append(doc.HydratedComponents, &astro.HydratedComponentMetadata{
						ExportName:   match.ExportName,
						Specifier:    match.Specifier,
						ResolvedPath: resolveIdForMatch(match, opts),
					})
				}

				break
			}
		}
	}
}

type ImportMatch struct {
	ExportName string
	Specifier  string
}

func matchNodeToImportStatement(doc *astro.Node, n *astro.Node) *ImportMatch {
	var match *ImportMatch

	eachImportStatement(doc, func(stmt js_scanner.ImportStatement) bool {
		for _, imported := range stmt.Imports {
			if imported.ExportName == "*" {
				prefix := fmt.Sprintf("%s.", imported.LocalName)

				if strings.HasPrefix(n.Data, prefix) {
					exportParts := strings.Split(n.Data[len(prefix):], ".")
					exportName := exportParts[0]

					match = &ImportMatch{
						ExportName: exportName,
						Specifier:  stmt.Specifier,
					}

					return false
				}
			} else if imported.LocalName == n.Data {
				match = &ImportMatch{
					ExportName: imported.ExportName,
					Specifier:  stmt.Specifier,
				}
				return false
			}
		}

		return true
	})

	return match
}

func resolveIdForMatch(match *ImportMatch, opts *TransformOptions) string {
	if strings.HasPrefix(match.Specifier, ".") && len(opts.Pathname) > 0 {
		u, err := url.Parse(opts.Pathname)
		if err == nil {
			ref, _ := url.Parse(match.Specifier)
			ou := u.ResolveReference(ref)
			return ou.String()
		}
	}
	return ""
}

func eachImportStatement(doc *astro.Node, cb func(stmt js_scanner.ImportStatement) bool) {
	if doc.FirstChild.Type == astro.FrontmatterNode {
		source := []byte(doc.FirstChild.FirstChild.Data)
		loc, statement := js_scanner.NextImportStatement(source, 0)
		for loc != -1 {
			if !cb(statement) {
				break
			}
			loc, statement = js_scanner.NextImportStatement(source, loc)
		}
	}
}

func walk(doc *astro.Node, cb func(*astro.Node)) {
	var f func(*astro.Node)
	f = func(n *astro.Node) {
		cb(n)
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
}
