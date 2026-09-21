package typescriptgen

import (
	"bytes"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/tooling/mrpc"
)

func parseCatalog(t *testing.T, path, text string, dependencies map[string]mrpc.Catalog) mrpc.Catalog {
	t.Helper()
	file, diagnostics := mrpc.Parse(mrpc.Source{Path: path, Text: text})
	if mrpc.HasErrors(diagnostics) {
		t.Fatalf("parse: %#v", diagnostics)
	}
	catalog, err := mrpc.NewCatalogWithDependencies([]mrpc.File{file}, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestGenerateTypedClientProviderResourcesAndExactCodecs(t *testing.T) {
	model := parseCatalog(t, "model.mrpc", `syntax = "mrpc/v2";
namespace example.model.v1;
option ts_module = "./model.js";
option ts_prefix = "API";
enum Mode { Ready = 0; Busy = 1; }
message Node { value int64 = 1; next optional[Node] = 2; }
resource Counter { Add(delta int64 = 1) returns (value int64 = 1); }
`, nil)
	service := parseCatalog(t, "service.mrpc", `syntax = "mrpc/v2";
namespace example.service.v1;
option ts_module = "./service.js";
import model "model.mrpc";
message Packet { bytes []uint8 = 1; values []int64 = 2; labels map[string]string = 3; maybe optional[[]uint8] = 4; }
service Laboratory { Open(node model.Node = 1) returns (counter model.Counter = 1, mode model.Mode = 2); }
`, map[string]mrpc.Catalog{"model.mrpc": model})

	first, err := Generate(service, Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate(service, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("TypeScript generation is not deterministic")
	}
	text := string(first)
	for _, expected := range []string{
		`import * as model from "./model.js"`,
		`bytes: Uint8Array | null`,
		`values: Array<bigint> | null`,
		`labels: Map<string, string> | null`,
		`maybe: Uint8Array | null | undefined`,
		`model.ApiNode`,
		`model.ApiCounterHandler | null`,
		`model.ApiCounterClient.from(this.binding`,
		`providedResource(`,
		`export function createLaboratoryProvider`,
		service.ContractID,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("generated TypeScript misses %q:\n%s", expected, text)
		}
	}
}

func TestGeneratePreservesIdentityAndRejectsInvalidTargets(t *testing.T) {
	catalog := parseCatalog(t, "sample.mrpc", `syntax = "mrpc/v2";
namespace sample.v1;
option ts_module = "./sample.js";
message Node { value int64 = 1; next optional[Node] = 2; }
service Tree { Echo(node Node = 1) returns (node Node = 1); }
`, nil)
	generated, err := Generate(catalog, Options{Runtime: "custom-runtime/rpc", Prefix: "Remote"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`from "custom-runtime/rpc"`,
		`export interface RemoteNode`,
		`lazyCodec(() => RemoteNodeCodec)`,
		catalog.ContractID,
	} {
		if !bytes.Contains(generated, []byte(expected)) {
			t.Fatalf("generated source misses %q", expected)
		}
	}
	catalog.ContractID = "mutated"
	if _, err := Generate(catalog, Options{}); err == nil {
		t.Fatal("stale catalog identity accepted")
	}
	catalog = parseCatalog(t, "sample.mrpc", `syntax = "mrpc/v2"; namespace sample.v1; option ts_module = "./sample.js"; message Item { value int64 = 1; }`, nil)
	for _, options := range []Options{{Runtime: "bad\nmodule"}, {Prefix: "bad-"}} {
		if _, err := Generate(catalog, options); err == nil {
			t.Fatalf("invalid options accepted: %#v", options)
		}
	}
}

func TestGenerateRejectsMissingImportsAndConvertedNameCollisions(t *testing.T) {
	dependency := parseCatalog(t, "model.mrpc", `syntax = "mrpc/v2"; namespace model.v1; message Item { value int64 = 1; }`, nil)
	root := parseCatalog(t, "service.mrpc", `syntax = "mrpc/v2"; namespace service.v1; option ts_module = "./service.js"; import model "model.mrpc"; service Service { Get(value model.Item = 1) returns (); }`, map[string]mrpc.Catalog{"model.mrpc": dependency})
	if _, err := Generate(root, Options{}); err == nil || !strings.Contains(err.Error(), "ts_module") {
		t.Fatalf("missing imported ts_module diagnostic: %v", err)
	}
	for _, declarations := range []string{
		`message FooBar { value int64 = 1; } message foo_bar { value int64 = 1; }`,
		`message Item { HTTPCode int64 = 1; http_code int64 = 2; }`,
		`service Service { close() returns (); }`,
		`service Service { constructor() returns (); }`,
		`service Service { binding() returns (); }`,
		`service Service { then() returns (); }`,
		`service Service { Echo(options int64 = 1) returns (); }`,
		`service Service { Echo(context int64 = 1) returns (); }`,
		`service Service { Echo(arguments int64 = 1) returns (); }`,
		`resource Counter { to_value() returns (); }`,
		`resource Counter { resource() returns (); }`,
		`service Foo { BarMethod() returns (); } service FooBar { Method() returns (); }`,
	} {
		catalog := parseCatalog(t, "collision.mrpc", `syntax = "mrpc/v2"; namespace collision.v1; option ts_module = "./collision.js"; `+declarations, nil)
		if _, err := Generate(catalog, Options{}); err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatalf("name collision not rejected for %q: %v", declarations, err)
		}
	}
	dependency.Options["ts_module"] = "./model.js"
	dependency.Options["ts_prefix"] = "bad-"
	root = parseCatalog(t, "service.mrpc", `syntax = "mrpc/v2"; namespace service.v1; option ts_module = "./service.js"; import model "model.mrpc"; service Service { Get(value model.Item = 1) returns (); }`, map[string]mrpc.Catalog{"model.mrpc": dependency})
	if _, err := Generate(root, Options{}); err == nil || !strings.Contains(err.Error(), "ts_prefix") {
		t.Fatalf("invalid imported ts_prefix diagnostic: %v", err)
	}
}
