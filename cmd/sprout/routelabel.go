package main

import (
	"errors"
	"fmt"
	"html"
	"strings"
)

var errRouteNotFound = errors.New("no instance matches that name")

// Ambiguity is only detectable at request time: a hostname carries no project
// scope to disambiguate by.
type routeAmbiguousError struct {
	label string
	ids   []string
}

func (e *routeAmbiguousError) Error() string {
	return fmt.Sprintf("name %q matches instances %s", e.label, strings.Join(e.ids, ", "))
}

// An ID prefix wins first: it is the escape hatch for an ambiguous name.
func resolveRouteLabel(label string) (string, error) {
	if id, err := matchIDPrefix(label); err != nil {
		// Re-cast so handle renders the same 409 page instead of a raw 500.
		var amb *idPrefixAmbiguousError
		if errors.As(err, &amb) {
			return "", &routeAmbiguousError{label: label, ids: amb.ids}
		}
		return "", err
	} else if id != "" {
		return id, nil
	}
	matches, err := instancesNamed(label)
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", errRouteNotFound
	case 1:
		return matches[0], nil
	default:
		return "", &routeAmbiguousError{label: label, ids: matches}
	}
}

// The rule every surface that hands out a route link obeys: take the
// sanitized name, then give it up for the ID once another instance answers to
// the same label, because resolveRouteLabel refuses a shared label as
// ambiguous and a link built from it could only 409.
func routeLabelAmong(id, name string, sharing func(label string) []string) (string, []string) {
	label := routeLabelOf(id, name)
	if label == id {
		return id, nil
	}
	if shared := sharing(label); len(shared) > 1 {
		return id, shared
	}
	return label, nil
}

// Lowercased because the router resolves labels case-insensitively: two names
// differing only in case are one route, so they have to meet as one label
// here rather than pass as two.
func routeLabelOf(id, name string) string {
	label, err := sanitizeName(name)
	if err != nil {
		return id
	}
	return strings.ToLower(label)
}

// Both maps are keyed by instance ID.
func routeLabels(names map[string]string) map[string]string {
	sharing := make(map[string][]string, len(names))
	for id, name := range names {
		label := routeLabelOf(id, name)
		sharing[label] = append(sharing[label], id)
	}
	labels := make(map[string]string, len(names))
	for id, name := range names {
		labels[id], _ = routeLabelAmong(id, name, func(label string) []string { return sharing[label] })
	}
	return labels
}

// The sharing set is a lookup by the sanitized label rather than the raw
// name, since distinct names sanitizing alike ("feature/foo", "feature-foo")
// are one label to the router and must fall back here too. A listing failure
// leaves the label standing: it is right whenever the label is in fact
// unique, where the ID would be wrong.
func routeLabelFor(id, name string) (label string, shared []string) {
	return routeLabelAmong(id, name, func(label string) []string {
		matches, err := instancesNamed(label)
		if err != nil {
			return nil
		}
		return matches
	})
}

func (e *routeAmbiguousError) hint(r *router) string {
	var b strings.Builder
	b.WriteString("Reach one by its ID instead:")
	for _, id := range e.ids {
		fmt.Fprintf(&b, "<br><code>%s</code> (%s)",
			html.EscapeString(r.urlFor(id)), html.EscapeString(displayForID(id)))
	}
	return b.String()
}
