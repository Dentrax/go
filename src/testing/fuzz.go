// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package testing

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"
)

func initFuzzFlags() {
	matchFuzz = flag.String("test.fuzz", "", "run the fuzz target matching `regexp`")
	isFuzzWorker = flag.Bool("test.fuzzworker", false, "coordinate with the parent process to fuzz random values")
}

var (
	matchFuzz    *string
	isFuzzWorker *bool
)

// InternalFuzzTarget is an internal type but exported because it is cross-package;
// it is part of the implementation of the "go test" command.
type InternalFuzzTarget struct {
	Name string
	Fn   func(f *F)
}

// F is a type passed to fuzz targets for fuzz testing.
type F struct {
	common
	context  *fuzzContext
	corpus   []corpusEntry // corpus is the in-memory corpus
	result   FuzzResult    // result is the result of running the fuzz target
	fuzzFunc func(f *F)    // fuzzFunc is the function which makes up the fuzz target
}

// corpus corpusEntry
type corpusEntry struct {
	b []byte
}

// Add will add the arguments to the seed corpus for the fuzz target. This will
// be a no-op if called after or within the Fuzz function. The args must match
// those in the Fuzz function.
func (f *F) Add(args ...interface{}) {
	if len(args) == 0 {
		panic("testing: Add must have at least one argument")
	}
	if len(args) != 1 {
		// TODO: support more than one argument
		panic("testing: Add only supports one argument currently")
	}
	switch v := args[0].(type) {
	case []byte:
		f.corpus = append(f.corpus, corpusEntry{v})
	// TODO: support other types
	default:
		panic("testing: Add only supports []byte currently")
	}
}

// Fuzz runs the fuzz function, ff, for fuzz testing. If ff fails for a set of
// arguments, those arguments will be added to the seed corpus.
//
// This is a terminal function which will terminate the currently running fuzz
// target by calling runtime.Goexit. To run any code after this function, use
// Cleanup.
func (f *F) Fuzz(ff interface{}) {
	fn, ok := ff.(func(*T, []byte))
	if !ok {
		panic("testing: Fuzz function must have type func(*testing.T, []byte)")
	}

	defer runtime.Goexit() // exit after this function

	var errStr string
	run := func(t *T, b []byte) {
		defer func() {
			err := recover()
			// If the function has recovered but the test hasn't finished,
			// it is due to a nil panic or runtime.GoExit.
			if !t.finished && err == nil {
				err = errNilPanicOrGoexit
			}
			if err != nil {
				t.Fail()
				t.output = []byte(fmt.Sprintf("    %s", err))
			}
			f.inFuzzFn = false
			t.signal <- true // signal that the test has finished
		}()
		// TODO(katiehockman, jayconrod): consider replacing inFuzzFn with
		// general purpose flag that checks whether specific methods can be
		// called.
		f.inFuzzFn = true
		fn(t, b)
		t.finished = true
	}

	switch {
	case f.context.coordinateFuzzing != nil:
		// Fuzzing is enabled, and this is the test process started by 'go test'.
		// Act as the coordinator process, and coordinate workers to perform the
		// actual fuzzing.
		seed := make([][]byte, len(f.corpus))
		for i, e := range f.corpus {
			seed[i] = e.b
		}
		err := f.context.coordinateFuzzing(*parallel, seed)
		f.setRan()
		f.finished = true
		f.result = FuzzResult{Error: err}
		// TODO(jayconrod,katiehockman): Aggregate statistics across workers
		// and set FuzzResult properly.

	case f.context.runFuzzWorker != nil:
		// Fuzzing is enabled, and this is a worker process. Follow instructions
		// from the coordinator.
		err := f.context.runFuzzWorker(func(input []byte) error {
			t := &T{
				common: common{
					signal: make(chan bool),
					w:      f.w,
					chatty: f.chatty,
				},
				context: newTestContext(1, nil),
			}
			go run(t, input)
			<-t.signal
			if t.Failed() {
				return errors.New(string(t.output))
			}
			return nil
		})
		if err != nil {
			// TODO(jayconrod,katiehockman): how should we handle a failure to
			// communicate with the coordinator? Might be caused by the coordinator
			// terminating early.
			fmt.Fprintf(os.Stderr, "testing: communicating with fuzz coordinator: %v\n", err)
			os.Exit(1)
		}
		f.setRan()
		f.finished = true

	default:
		// Fuzzing is not enabled. Only run the seed corpus.
		for _, c := range f.corpus {
			t := &T{
				common: common{
					signal: make(chan bool),
					w:      f.w,
					chatty: f.chatty,
				},
				context: newTestContext(1, nil),
			}
			go run(t, c.b)
			<-t.signal
			if t.Failed() {
				f.Fail()
				errStr += string(t.output)
			}
			f.setRan()
		}
		f.finished = true
		if f.Failed() {
			f.result = FuzzResult{Error: errors.New(errStr)}
			return
		}
	}
}

func (f *F) report() {
	if f.Failed() {
		fmt.Fprintf(f.w, "--- FAIL: %s\n%s\n", f.name, f.result.String())
	} else if f.chatty != nil {
		if f.Skipped() {
			f.chatty.Updatef(f.name, "SKIP\n")
		} else {
			f.chatty.Updatef(f.name, "PASS\n")
		}
	}
}

// run runs each fuzz target in its own goroutine with its own *F.
func (f *F) run(name string, fn func(f *F)) (ran, ok bool) {
	innerF := &F{
		common: common{
			signal: make(chan bool),
			name:   name,
			chatty: f.chatty,
			w:      f.w,
		},
		context: f.context,
	}
	if innerF.chatty != nil {
		innerF.chatty.Updatef(name, "=== RUN   %s\n", name)
	}
	go innerF.runTarget(fn)
	<-innerF.signal
	return innerF.ran, !innerF.failed
}

// runTarget runs the given target, handling panics and exits
// within the test, and reporting errors.
func (f *F) runTarget(fn func(*F)) {
	defer func() {
		err := recover()
		// If the function has recovered but the test hasn't finished,
		// it is due to a nil panic or runtime.GoExit.
		if !f.finished && err == nil {
			err = errNilPanicOrGoexit
		}
		if err != nil {
			f.Fail()
			f.result = FuzzResult{Error: fmt.Errorf("    %s", err)}
		}
		f.report()
		f.setRan()
		f.signal <- true // signal that the test has finished
	}()
	defer f.runCleanup(normalPanic)
	fn(f)
	f.finished = true
}

// FuzzResult contains the results of a fuzz run.
type FuzzResult struct {
	N       int           // The number of iterations.
	T       time.Duration // The total time taken.
	Crasher *corpusEntry  // Crasher is the corpus entry that caused the crash
	Error   error         // Error is the error from the crash
}

func (r FuzzResult) String() string {
	s := ""
	if r.Error == nil {
		return s
	}
	s = fmt.Sprintf("%s", r.Error.Error())
	if r.Crasher != nil {
		s += fmt.Sprintf("\ncrasher: %b", r.Crasher)
	}
	return s
}

// fuzzContext holds all fields that are common to all fuzz targets.
type fuzzContext struct {
	runMatch          *matcher
	fuzzMatch         *matcher
	coordinateFuzzing func(int, [][]byte) error
	runFuzzWorker     func(func([]byte) error) error
}

// runFuzzTargets runs the fuzz targets matching the pattern for -run. This will
// only run the f.Fuzz function for each seed corpus without using the fuzzing
// engine to generate or mutate inputs.
func runFuzzTargets(matchString func(pat, str string) (bool, error), fuzzTargets []InternalFuzzTarget) (ran, ok bool) {
	ok = true
	if len(fuzzTargets) == 0 || *isFuzzWorker {
		return ran, ok
	}
	ctx := &fuzzContext{runMatch: newMatcher(matchString, *match, "-test.run")}
	var fts []InternalFuzzTarget
	for _, ft := range fuzzTargets {
		if _, matched, _ := ctx.runMatch.fullName(nil, ft.Name); matched {
			fts = append(fts, ft)
		}
	}
	f := &F{
		common: common{
			w: os.Stdout,
		},
		fuzzFunc: func(f *F) {
			for _, ft := range fts {
				// Run each fuzz target in it's own goroutine.
				ftRan, ftOk := f.run(ft.Name, ft.Fn)
				ran = ran || ftRan
				ok = ok && ftOk
			}
		},
		context: ctx,
	}
	if Verbose() {
		f.chatty = newChattyPrinter(f.w)
	}
	f.fuzzFunc(f)
	return ran, ok
}

// runFuzzing runs the fuzz target matching the pattern for -fuzz. Only one such
// fuzz target must match. This will run the fuzzing engine to generate and
// mutate new inputs against the f.Fuzz function.
//
// If fuzzing is disabled (-test.fuzz is not set), runFuzzing
// returns immediately.
func runFuzzing(deps testDeps, fuzzTargets []InternalFuzzTarget) (ran, ok bool) {
	if len(fuzzTargets) == 0 || *matchFuzz == "" {
		return false, true
	}
	ctx := &fuzzContext{fuzzMatch: newMatcher(deps.MatchString, *matchFuzz, "-test.fuzz")}
	if *isFuzzWorker {
		ctx.runFuzzWorker = deps.RunFuzzWorker
	} else {
		ctx.coordinateFuzzing = deps.CoordinateFuzzing
	}
	f := &F{
		common: common{
			signal: make(chan bool),
			w:      os.Stdout,
		},
		context: ctx,
	}
	var (
		ft    InternalFuzzTarget
		found int
	)
	for _, ft = range fuzzTargets {
		testName, matched, _ := ctx.fuzzMatch.fullName(&f.common, ft.Name)
		if matched {
			found++
			if found > 1 {
				fmt.Fprintln(os.Stderr, "testing: warning: -fuzz matches more than one target, won't fuzz")
				return false, true
			}
			f.name = testName
		}
	}
	if found == 0 {
		return false, true
	}
	if Verbose() {
		f.chatty = newChattyPrinter(f.w)
		f.chatty.Updatef(f.name, "--- FUZZ: %s\n", f.name)
	}
	go f.runTarget(ft.Fn)
	<-f.signal
	return f.ran, !f.failed
}

// Fuzz runs a single fuzz target. It is useful for creating
// custom fuzz targets that do not use the "go test" command.
//
// If fn depends on testing flags, then Init must be used to register
// those flags before calling Fuzz and before calling flag.Parse.
func Fuzz(fn func(f *F)) FuzzResult {
	f := &F{
		common: common{
			w: discard{},
		},
		fuzzFunc: fn,
	}
	// TODO(katiehockman): run the test
	return f.result
}