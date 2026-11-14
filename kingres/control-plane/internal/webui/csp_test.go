// SPDX-License-Identifier: PostgreSQL

package webui

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// The Content-Security-Policy of kingresd (internal/httpapi/headers.go) allows
// only 'self' sources and no inline script or style. These tests scan what the
// panel ships and fail on anything the policy would block.
var (
	reInlineScript = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	reSrcAttr      = regexp.MustCompile(`(?i)\bsrc\s*=`)
	reEventAttr    = regexp.MustCompile(`(?i)<[a-z][^>]*\son[a-z]+\s*=`)
	reStyleAttr    = regexp.MustCompile(`(?i)<[a-z][^>]*\sstyle\s*=`)
	reStyleTag     = regexp.MustCompile(`(?i)<style\b`)
	reJSURL        = regexp.MustCompile(`(?i)javascript:`)
	reExternalAttr = regexp.MustCompile(`(?i)\b(?:src|href|action|poster|data)\s*=\s*["']?\s*(?:https?:)?//`)
	reMetaRefresh  = regexp.MustCompile(`(?i)http-equiv\s*=\s*["']?refresh`)
	reCSSURL       = regexp.MustCompile(`(?i)url\(\s*["']?([^)"']*)`)
	reCSSImport    = regexp.MustCompile(`(?i)@import\s+["']?([^;"')]*)`)
)

// policyViolations lists what in fsys the strict policy would block.
func policyViolations(fsys fs.FS) []string {
	var out []string
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			return nil
		}
		text := string(b)
		switch strings.ToLower(path.Ext(p)) {
		case ".html", ".htm":
			for _, m := range reInlineScript.FindAllStringSubmatch(text, -1) {
				if !reSrcAttr.MatchString(m[1]) || strings.TrimSpace(m[2]) != "" {
					out = append(out, p+": inline script")
				}
			}
			if reEventAttr.MatchString(text) {
				out = append(out, p+": on*= event handler attribute")
			}
			if reStyleAttr.MatchString(text) {
				out = append(out, p+": style= attribute")
			}
			if reStyleTag.MatchString(text) {
				out = append(out, p+": inline <style>")
			}
			if reJSURL.MatchString(text) {
				out = append(out, p+": javascript: URL")
			}
			if reExternalAttr.MatchString(text) {
				out = append(out, p+": external origin")
			}
			if reMetaRefresh.MatchString(text) {
				out = append(out, p+": meta refresh")
			}
		case ".css":
			for _, m := range reCSSURL.FindAllStringSubmatch(text, -1) {
				if u := strings.ToLower(strings.TrimSpace(m[1])); strings.HasPrefix(u, "data:") || strings.Contains(u, "//") {
					out = append(out, p+": css url("+m[1]+") is external or data:")
				}
			}
			for _, m := range reCSSImport.FindAllStringSubmatch(text, -1) {
				if strings.Contains(m[1], "//") {
					out = append(out, p+": css @import "+m[1])
				}
			}
		}
		return nil
	})
	return out
}

// viteFixture mimics the output of a Vite build of the panel.
func viteFixture() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte(`<!doctype html><html lang="en"><head><meta charset="UTF-8" />
<link rel="icon" type="image/svg+xml" href="/favicon.svg" />
<link rel="manifest" href="/manifest.webmanifest" />
<script type="module" crossorigin src="/assets/index-27aDquRx.js"></script>
<link rel="modulepreload" crossorigin href="/assets/framework-B8IFayqN.js">
<link rel="stylesheet" crossorigin href="/assets/index-CDEl9io4.css">
</head><body><div id="app"></div></body></html>`)},
		"assets/index-27aDquRx.js":     {Data: []byte(`import"./framework-B8IFayqN.js";console.log(1)`)},
		"assets/framework-B8IFayqN.js": {Data: []byte(`export const a=1`)},
		"assets/index-CDEl9io4.css":    {Data: []byte(`body{margin:0}.x{background:url(/img/a.svg)}`)},
	}
}

func TestPolicyScanAcceptsViteBuild(t *testing.T) {
	if v := policyViolations(viteFixture()); len(v) != 0 {
		t.Fatalf("Vite fixture: %v", v)
	}
}

func TestPolicyScanDetectsViolations(t *testing.T) {
	bad := map[string]string{
		"inline script":  `<script>alert(1)</script>`,
		"event handler":  `<body onload="x()">`,
		"style attr":     `<div style="color:red">`,
		"inline style":   `<style>b{}</style>`,
		"javascript url": `<a href="javascript:x()">`,
		"external":       `<script src="https://cdn.example.com/a.js"></script>`,
		"protocol rel":   `<link rel="stylesheet" href="//cdn.example.com/a.css">`,
	}
	for name, html := range bad {
		if v := policyViolations(fstest.MapFS{"index.html": {Data: []byte(html)}}); len(v) == 0 {
			t.Errorf("%s: not detected", name)
		}
	}
	for name, css := range map[string]string{
		"external url": `a{background:url(https://x.example/a.png)}`,
		"data url":     `a{background:url(data:image/png;base64,AAAA)}`,
		"import":       `@import url("https://x.example/a.css");`,
	} {
		if v := policyViolations(fstest.MapFS{"a.css": {Data: []byte(css)}}); len(v) == 0 {
			t.Errorf("%s: not detected", name)
		}
	}
}

// TestEmbeddedPanelFitsPolicy scans the panel embedded into this binary (the
// placeholder or the real build).
func TestEmbeddedPanelFitsPolicy(t *testing.T) {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		t.Fatal(err)
	}
	if v := policyViolations(sub); len(v) != 0 {
		t.Fatalf("embedded panel violates the Content-Security-Policy: %v", v)
	}
}

func TestIsHashedAsset(t *testing.T) {
	for p, want := range map[string]bool{
		"/assets/index-27aDquRx.js":      true,
		"/assets/AppDialog-DcEveFC2.css": true,
		"/assets/a-b-c-Ab3_-x9Z.js":      true,
		"/assets/app.js":                 false,
		"/assets/my-document.js":         false, // 8 lower case letters are no hash
		"/assets/index-27aDqu.js":        false, // hash too short
		"/assets/index-27aDquRx":         false, // no extension
		"/index-27aDquRx.js":             false, // not below /assets/
		"/assets/sub/index-27aDquRx.js":  false,
		"/favicon.svg":                   false,
	} {
		if got := IsHashedAsset(p); got != want {
			t.Errorf("IsHashedAsset(%q) = %v, want %v", p, got, want)
		}
	}
}
