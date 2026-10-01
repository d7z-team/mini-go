package bootstrap_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/bootstrap/compilerentry"
	rt "github.com/d7z-team/mini-go/runtime"
)

func BenchmarkCompilerImagePhases(b *testing.B) {
	image := buildCompilerImage(b)
	b.Run("load", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := rt.LoadExecutionImage(image); err != nil {
				b.Fatal(err)
			}
		}
	})
	program, err := rt.LoadExecutionImage(image)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("instantiate", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			instance, err := program.Instantiate(context.Background(), compilerImageInstanceOptions())
			if err != nil {
				b.Fatal(err)
			}
			if err := instance.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})

	for _, operation := range []compilerentry.Operation{compilerentry.OperationCheck, compilerentry.OperationPrepare} {
		request := compilerentry.Request{
			Format:    compilerentry.ServiceFormat,
			Version:   compilerentry.ServiceVersion,
			Operation: operation,
			Root:      "benchmark/main",
			Packages: []compilerentry.Package{{
				Namespace:  "module:benchmark",
				ModulePath: "benchmark/main",
				Files: []compilerentry.File{{
					Path: "main.mgo", Text: "package main\nfunc Main() int { return 42 }\n",
				}},
			}},
			EntryPoints: []compiler.EntryPoint{{Name: "main", ModulePath: "benchmark/main", Function: "Main"}},
		}
		input, err := json.Marshal(request)
		if err != nil {
			b.Fatal(err)
		}
		for _, warm := range []bool{false, true} {
			name := string(operation) + "/cold"
			if warm {
				name = string(operation) + "/warm"
			}
			b.Run(name, func(b *testing.B) {
				var instance *rt.Instance
				if warm {
					instance, err = program.Instantiate(context.Background(), compilerImageInstanceOptions())
					if err != nil {
						b.Fatal(err)
					}
					b.Cleanup(func() { _ = instance.Close() })
				}
				var steps, guestBytes int64
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if !warm {
						instance, err = program.Instantiate(context.Background(), compilerImageInstanceOptions())
						if err != nil {
							b.Fatal(err)
						}
					}
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					before, err := instance.RuntimeStats(ctx)
					if err != nil {
						cancel()
						b.Fatal(err)
					}
					execution, err := instance.Start("default", rt.HostBytes(input))
					if err != nil {
						cancel()
						b.Fatal(err)
					}
					result, err := execution.Wait(ctx)
					if err != nil {
						cancel()
						b.Fatal(err)
					}
					scope, err := execution.WaitScope(ctx)
					if err != nil {
						cancel()
						b.Fatal(err)
					}
					after, err := instance.RuntimeStats(ctx)
					cancel()
					if err != nil {
						b.Fatal(err)
					}
					steps += scope.Steps
					guestBytes += after.TotalAllocatedBytes - before.TotalAllocatedBytes
					output, ok := result.Values[0].Bytes()
					if !ok {
						b.Fatal("compiler result is not bytes")
					}
					var response compilerentry.Response
					if err := json.Unmarshal(output, &response); err != nil {
						b.Fatal(err)
					}
					if response.Error != "" || len(response.Diagnostics) != 0 || operation == compilerentry.OperationPrepare && response.Image == nil {
						b.Fatalf("compiler response: error=%q diagnostics=%v image=%v", response.Error, response.Diagnostics, response.Image != nil)
					}
					if !warm {
						if err := instance.Close(); err != nil {
							b.Fatal(err)
						}
					}
				}
				b.ReportMetric(float64(steps)/float64(b.N), "steps/op")
				b.ReportMetric(float64(guestBytes)/float64(b.N), "guest-bytes/op")
				b.ReportMetric(float64(steps)/b.Elapsed().Seconds(), "steps/s")
			})
		}
	}
}
