package drydock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRef(t *testing.T) {
	if got := NormalizeRef("github:foo/bar"); got != "github:foo/bar" {
		t.Errorf("flake ref changed: %q", got)
	}
	if got := NormalizeRef("./demo"); !strings.HasPrefix(got, "path:/") {
		t.Errorf("relative path not normalized: %q", got)
	}
	if got := NormalizeRef("/abs/demo"); got != "path:/abs/demo" {
		t.Errorf("absolute path not normalized: %q", got)
	}
}

func TestNixQuote(t *testing.T) {
	cases := map[string]string{
		"plain": `"plain"`,
		`a"b`:   `"a\"b"`,
		`a\b`:   `"a\\b"`,
		"${x}":  `"\${x}"`,
	}
	for in, want := range cases {
		if got := nixQuote(in); got != want {
			t.Errorf("nixQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSplitPassthrough(t *testing.T) {
	overrides, nixArgs, rest, err := splitPassthrough([]string{
		"up",
		"--override-input", "my-nix", "../my-nix",
		"--nix-arg=--option",
		"--override-input=other=path:./x",
		"--headless",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 2 {
		t.Fatalf("overrides = %#v", overrides)
	}
	if overrides[0].Name != "my-nix" || !strings.HasPrefix(overrides[0].Ref, "path:") {
		t.Errorf("override[0] = %#v", overrides[0])
	}
	if overrides[1].Name != "other" || overrides[1].Ref != "path:./x" {
		t.Errorf("override[1] = %#v", overrides[1])
	}
	if len(nixArgs) != 1 || nixArgs[0] != "--option" {
		t.Errorf("nixArgs = %#v", nixArgs)
	}
	if got := strings.Join(rest, " "); got != "up --headless" {
		t.Errorf("rest = %q", got)
	}

	// `--` must stop parsing so ssh commands are untouched.
	_, _, rest, err = splitPassthrough([]string{"ssh", "--", "--override-input", "x", "y"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rest, " "); got != "ssh -- --override-input x y" {
		t.Errorf("rest after -- = %q", got)
	}
}

func TestApplyPathMap(t *testing.T) {
	mapping := [][2]string{{"/guest/dev", "/host/dev"}}
	if got := applyPathMap("path:/guest/dev/my-nix", mapping); got != "path:/host/dev/my-nix" {
		t.Errorf("mapped ref = %q", got)
	}
	if got := applyPathMap("/guest/dev", mapping); got != "/host/dev" {
		t.Errorf("mapped exact = %q", got)
	}
	if got := applyPathMap("/elsewhere", mapping); got != "/elsewhere" {
		t.Errorf("unmapped = %q", got)
	}
}

func TestDecodePPM(t *testing.T) {
	data := append([]byte("P6\n2 1\n255\n"), 255, 0, 0, 0, 255, 0)
	img, err := decodePPM(data)
	if err != nil {
		t.Fatal(err)
	}
	if bounds := img.Bounds(); bounds.Dx() != 2 || bounds.Dy() != 1 {
		t.Fatalf("bounds = %v", bounds)
	}
	red, green, _, _ := img.At(0, 0).RGBA()
	if red>>8 != 255 || green>>8 != 0 {
		t.Errorf("pixel(0,0) = %d,%d", red>>8, green>>8)
	}
}

func TestResolvePrecedence(t *testing.T) {
	engine := &Engine{
		StateRoot:    t.TempDir(),
		DefaultFlake: "path:/default",
		DefaultHost:  "default-host",
		DefaultOverrides: []InputOverride{
			{Name: "marker", Ref: "path:/default-marker"},
		},
	}

	// No saved metadata: portal defaults apply.
	got, err := engine.resolve(Request{Name: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	if got.req.Flake != "path:/default" || got.req.Host != "default-host" {
		t.Errorf("defaults not applied: %#v", got.req)
	}
	if len(got.req.Overrides) != 1 || got.req.Overrides[0].Ref != "path:/default-marker" {
		t.Errorf("default overrides not applied: %#v", got.req.Overrides)
	}

	// Saved VM metadata wins over portal defaults.
	if err := engine.saveMeta(&meta{
		Name: "vm", Flake: "path:/saved", Host: "saved-host",
		Overrides: []InputOverride{{Name: "marker", Ref: "path:/saved-marker"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = engine.resolve(Request{Name: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	if got.req.Flake != "path:/saved" || got.req.Host != "saved-host" {
		t.Errorf("saved state did not win: %#v", got.req)
	}
	if len(got.req.Overrides) != 1 || got.req.Overrides[0].Ref != "path:/saved-marker" {
		t.Errorf("saved overrides did not win: %#v", got.req.Overrides)
	}

	// Explicit request wins over everything.
	got, err = engine.resolve(Request{
		Name: "vm", Flake: "path:/explicit", Host: "explicit-host",
		Overrides: []InputOverride{{Name: "marker", Ref: "path:/explicit-marker"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.req.Flake != "path:/explicit" || got.req.Host != "explicit-host" {
		t.Errorf("explicit request did not win: %#v", got.req)
	}
	if len(got.req.Overrides) != 1 || got.req.Overrides[0].Ref != "path:/explicit-marker" {
		t.Errorf("explicit overrides did not win: %#v", got.req.Overrides)
	}
}

func TestWriteWrapper(t *testing.T) {
	dir := t.TempDir()
	engine := &Engine{NixDir: "/nix/store/aaa-source/nix"}
	if err := engine.writeWrapper(dir, Request{Flake: "path:/x", Host: "demo"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "flake.nix"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`inputs.config.url = "path:/x";`,
		`builtins.getAttr "demo"`,
		"/nix/store/aaa-source/nix/guest",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("wrapper missing %q:\n%s", want, data)
		}
	}
}
