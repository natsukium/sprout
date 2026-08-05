package main

import (
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
)

func portSuffix(port int) string {
	if port == 80 {
		return ""
	}
	return ":" + strconv.Itoa(port)
}

// Shaped like routedURL's output with <name> where the label goes, so a
// message and the links the router hands out cannot drift apart.
func routeURLTemplate(domain string, port int) string {
	return "http://<name>." + domain + portSuffix(port) + "/"
}

func (r *router) urlFor(label string) string {
	return routedURL("", label, r.domain, 0, r.port)
}

// Reads on-disk records only: a typo should not fork a process per instance
// just to suggest names.
func (r *router) knownNamesHint() string {
	ids, err := instanceIDs()
	if err != nil || len(ids) == 0 {
		return "No instances exist yet — <code>sprout up</code> one first."
	}
	names := make(map[string]string, len(ids))
	for _, id := range ids {
		names[id] = id
		if inst, _, lerr := loadInstance(id); lerr == nil {
			names[id] = inst.Name
		}
	}
	labels := routeLabels(names)
	var b strings.Builder
	b.WriteString("Known instances:")
	for _, id := range ids {
		fmt.Fprintf(&b, "<br><code>%s</code>", html.EscapeString(r.urlFor(labels[id])))
	}
	return b.String()
}

// Unlike knownNamesHint this pays instanceRows' per-instance queries for
// the state column, which a page a human explicitly opened can afford.
func (r *router) writeIndex(conn net.Conn) {
	ids, err := instanceIDs()
	var body strings.Builder
	if err != nil {
		fmt.Fprintf(&body, "<p>Could not list instances: %s</p>", html.EscapeString(err.Error()))
	} else if rows := instanceRows(ids); len(rows) == 0 {
		body.WriteString("<p>No instances. <code>sprout up</code> one, then it appears here.</p>")
	} else {
		names := make(map[string]string, len(rows))
		for _, row := range rows {
			names[row.ID] = row.Name
		}
		labels := routeLabels(names)
		body.WriteString("<table><tr><th>name</th><th>state</th><th>link</th></tr>")
		for _, row := range rows {
			url := r.urlFor(labels[row.ID])
			fmt.Fprintf(&body, "<tr><td>%s</td><td>%s</td><td><a href=\"%s\">%s</a></td></tr>",
				html.EscapeString(row.Name), html.EscapeString(row.State), html.EscapeString(url), html.EscapeString(url))
		}
		body.WriteString("</table>")
	}
	r.writePage(conn, http.StatusOK, nil, "sprout route", body.String())
}

// The Refresh header, not a script, drives the reload, so a non-browser
// client still gets an honest 503 with Retry-After.
func (r *router) writeWaking(conn net.Conn, name string) {
	body := fmt.Sprintf("<p>Waking <strong>%s</strong> — this page reloads until it answers.</p>", html.EscapeString(name))
	r.writePage(conn, http.StatusServiceUnavailable,
		map[string]string{"Refresh": "2", "Retry-After": "2"}, "starting "+name, body)
}

// detail is raw HTML, so callers pass pre-escaped markup.
func (r *router) writeError(conn net.Conn, status int, title, detail string) {
	body := "<p>" + html.EscapeString(title) + "</p>"
	if detail != "" {
		body += "<p class=\"hint\">" + detail + "</p>"
	}
	r.writePage(conn, status, nil, http.StatusText(status), body)
}

// Marks a response as the router's own, so a 404 from here is told apart from
// one the guest's ingress wrote. Nothing bridged carries it.
const routeServerHeader = "sprout-route"

func (r *router) writePage(conn net.Conn, status int, headers map[string]string, title, body string) {
	page := "<!doctype html><html><head><meta charset=\"utf-8\"><title>" + html.EscapeString(title) +
		"</title><style>" + routeCSS + "</style></head><body><main>" +
		"<h1>sprout route</h1>" + body + "</main></body></html>\n"
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	fmt.Fprint(&b, "Content-Type: text/html; charset=utf-8\r\n")
	fmt.Fprintf(&b, "Content-Length: %d\r\n", len(page))
	fmt.Fprintf(&b, "Server: %s\r\n", routeServerHeader)
	fmt.Fprint(&b, "Connection: close\r\n")
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	b.WriteString(page)
	_, _ = io.WriteString(conn, b.String())
}

const routeCSS = `body{font:15px/1.5 system-ui,sans-serif;margin:0;background:#f6f7f9;color:#1a1a1a}` +
	`main{max-width:42rem;margin:12vh auto;padding:2rem}` +
	`h1{font-size:.8rem;text-transform:uppercase;letter-spacing:.08em;color:#888;margin:0 0 1rem}` +
	`p{margin:.5rem 0}.hint{color:#555;font-size:.92rem}` +
	`code{background:#e8eaed;padding:.1em .35em;border-radius:4px;font-size:.9em}` +
	`a{color:#2563eb}table{border-collapse:collapse;margin-top:1rem}` +
	`th,td{text-align:left;padding:.3rem .9rem .3rem 0}th{color:#888;font-weight:600;font-size:.8rem}` +
	`@media(prefers-color-scheme:dark){body{background:#16181d;color:#e6e6e6}code{background:#2a2d34}a{color:#7aa2f7}.hint{color:#aaa}}`
