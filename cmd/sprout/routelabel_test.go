package main

import (
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
)

// Names are host-wide, so two clones on the same branch answer to one name.
// That name is refused as ambiguous, and every surface handing out a link
// falls back to the ID that still addresses one instance.
func TestRouteLabelsFallBackToTheIDForASharedName(t *testing.T) {
	labels := routeLabels(map[string]string{
		"aaaa00000001": "main",
		"bbbb00000002": "main",
		"cccc00000003": "feat/login",
	})
	for _, id := range []string{"aaaa00000001", "bbbb00000002"} {
		if labels[id] != id {
			t.Errorf("label for %s is %q, want its ID: the shared name routes to neither instance", id, labels[id])
		}
	}
	if labels["cccc00000003"] != "feat-login" {
		t.Errorf("label for the uniquely named instance is %q, want the sanitized name", labels["cccc00000003"])
	}
}

// Names differing only in case reach the same route (the label is lowercased
// on the way into the URL), so they collide like identical ones.
func TestRouteLabelsTreatCaseVariantsAsTheSameName(t *testing.T) {
	labels := routeLabels(map[string]string{
		"aaaa00000001": "Main",
		"bbbb00000002": "main",
	})
	for id, label := range labels {
		if label != id {
			t.Errorf("label for %s is %q, want its ID: %q and %q reach the same route", id, label, "Main", "main")
		}
	}
}

// Sanitizing is lossy, so two distinct names can answer to one label. The
// label producers must fall back to the ID exactly when the router would
// refuse the label as ambiguous — from both colliding names, not just the
// one whose raw form matches the label.
func TestRouteLabelForFallsBackToTheIDWhenSanitizedLabelsCollide(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	newTestInstance(t, root, "aaaa00000001", "feature/foo", "")
	newTestInstance(t, root, "bbbb00000002", "feature-foo", "")

	for id, name := range map[string]string{
		"aaaa00000001": "feature/foo",
		"bbbb00000002": "feature-foo",
	} {
		label, shared := routeLabelFor(id, name)
		if label != id {
			t.Errorf("label for %q is %q, want its ID: the router refuses the shared label as ambiguous", name, label)
		}
		if len(shared) != 2 {
			t.Errorf("shared for %q lists %d instances, want both colliding ones", name, len(shared))
		}
	}
}

// The router labels a whole listing at once while `open` and `inspect` label
// the one instance they hold. Both address the same instance, so a
// disagreement would make the link the index hands out and the one a script
// prints point at different places.
func TestRouteLabelSurfacesAgree(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	names := map[string]string{
		"aaaa00000001": "feature/foo",     // collides with the next one
		"bbbb00000002": "feature-foo",     // once sanitized
		"cccc00000003": "Main",            // unique, but not lowercase
		"dddd00000004": "release/2024.q3", // unique, and dotted
	}
	for id, name := range names {
		newTestInstance(t, root, id, name, "")
	}

	labels := routeLabels(names)
	for id, name := range names {
		single, _ := routeLabelFor(id, name)
		if single != labels[id] {
			t.Errorf("%q labels as %q for one instance but %q in a listing", name, single, labels[id])
		}
	}
}

// The index page is the surface that hands links to a browser, so a
// duplicated name must not render two rows pointing at the same 409.
func TestRouterIndexLinksASharedNameByID(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	newTestInstance(t, root, "aaaa00000001", "main", "")
	newTestInstance(t, root, "bbbb00000002", "main", "")

	r := &router{domain: defaultRouteDomain, port: 8080}
	client, server := net.Pipe()
	go func() { r.writeIndex(server); server.Close() }()
	raw, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)

	if strings.Contains(page, "http://main.") {
		t.Errorf("index links the ambiguous name:\n%s", page)
	}
	for _, id := range []string{"aaaa00000001", "bbbb00000002"} {
		if !strings.Contains(page, "http://"+id+".") {
			t.Errorf("index does not link instance %s by ID:\n%s", id, page)
		}
	}
}

// --print feeds a script, so printing the name URL for a duplicated name
// would hand the script an address the router answers 409 for.
func TestOpenPrintsTheIDURLForASharedName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	newTestInstance(t, root, "aaaa00000001", "main", "")
	newTestInstance(t, root, "bbbb00000002", "main", "")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	out := captureStdout(t, func() error {
		return cmdOpen("aaaa00000001", 0, port, defaultRouteDomain, "", true)
	})
	want := "http://aaaa00000001.sprout.localhost:" + strconv.Itoa(port) + "/"
	if strings.TrimSpace(out) != want {
		t.Errorf("printed %q, want %q — the shared name would 409", strings.TrimSpace(out), want)
	}
}
