package stdlib_test

import (
	"errors"
	"testing"
	"time"

	"github.com/d7z-team/mini-go/compiler"
	miniruntime "github.com/d7z-team/mini-go/runtime"
)

type uuidEntropy struct {
	fail  bool
	reads int
}

func (reader *uuidEntropy) Read(data []byte) (int, error) {
	reader.reads++
	if reader.fail {
		return 0, errors.New("uuid entropy unavailable")
	}
	for i := range data {
		data[i] = 0x5a
	}
	return len(data), nil
}

func TestUUIDCancelDuringEntropyLeavesInstanceReusable(t *testing.T) {
	program := prepareStdlibProgram(t, "example/uuid-cancel", `package main
import "uuid"
func Generate() string { return uuid.NewV7().String() }
`, []compiler.EntryPoint{{Name: "v7", Function: "Generate"}})
	entropy := &uuidEntropy{}
	instance, err := program.Instantiate(t.Context(), miniruntime.InstanceOptions{Entropy: entropy})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	execution, err := instance.Start("v7")
	if err != nil {
		t.Fatal(err)
	}
	for steps := 0; entropy.reads == 0 && steps < 10000; steps++ {
		if _, _, err := execution.PollSteps(1); err != nil {
			t.Fatal(err)
		}
	}
	if entropy.reads == 0 {
		t.Fatal("UUID generation did not reach entropy read")
	}
	execution.Cancel()
	if execution.State() != miniruntime.ExecutionCanceled {
		t.Fatalf("canceled UUID state: %v", execution.State())
	}
	result, err := instance.Call(t.Context(), "v7")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].StringValue(); !ok || len(value) != 36 {
		t.Fatalf("UUID after canceled entropy read: %v", result.Values)
	}
}

type uuidClock struct {
	*miniruntime.ManualClock
	now time.Time
}

func (clock *uuidClock) Now() time.Time { return clock.now }

func TestUUIDHostFailureClockAndInstanceState(t *testing.T) {
	program := prepareStdlibProgram(t, "example/uuid-host", `package main
import "uuid"
func Generate() (text string) {
 defer func() { if recover() != nil { text = "entropy failure" } }()
 return uuid.NewV7().String()
}
func Random() (text string) {
 defer func() { if recover() != nil { text = "entropy failure" } }()
 return uuid.NewV4().String()
}
`, []compiler.EntryPoint{{Name: "v7", Function: "Generate"}, {Name: "v4", Function: "Random"}})
	clock := &uuidClock{ManualClock: miniruntime.NewManualClock(time.Unix(100, 0)), now: time.Unix(100, 0)}
	entropy := &uuidEntropy{fail: true}
	instance, err := program.Instantiate(t.Context(), miniruntime.InstanceOptions{Clock: clock, Entropy: entropy})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	for _, entry := range []string{"v4", "v7"} {
		result, err := instance.Call(t.Context(), entry)
		if err != nil {
			t.Fatal(err)
		}
		text, ok := result.Values[0].StringValue()
		if !ok || text != "entropy failure" {
			t.Fatalf("%s accepted entropy failure", entry)
		}
	}
	entropy.fail = false
	generate := func(vm *miniruntime.Instance) string {
		t.Helper()
		result, err := vm.Call(t.Context(), "v7")
		if err != nil {
			t.Fatal(err)
		}
		value, ok := result.Values[0].StringValue()
		if !ok {
			t.Fatal("UUID result is not text")
		}
		return value
	}
	first := generate(instance)
	other, err := program.Instantiate(t.Context(), miniruntime.InstanceOptions{Clock: clock, Entropy: &uuidEntropy{}})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if isolated := generate(other); isolated != first {
		t.Fatalf("failed entropy advanced state or state shared: %s != %s", isolated, first)
	}
	if second := generate(instance); second <= first {
		t.Fatal("same-time UUIDs not ordered")
	}
	clock.now = time.Unix(99, 0)
	if rollback := generate(instance); rollback >= first {
		t.Fatal("clock rollback did not follow timestamp")
	}
}
