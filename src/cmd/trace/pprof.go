// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Serving of pprof-like profiles.

package main

import (
	"bufio"
	"cmd/internal/pprof/profile"
	"fmt"
	"internal/trace"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
)

func init() {
	http.HandleFunc("/io", serveSVGProfile(pprofIO))
	http.HandleFunc("/block", serveSVGProfile(pprofBlock))
	http.HandleFunc("/syscall", serveSVGProfile(pprofSyscall))
	http.HandleFunc("/sched", serveSVGProfile(pprofSched))
}

// Record represents one entry in pprof-like profiles.
type Record struct {
	stk  []*trace.Frame
	n    uint64
	time int64
}

// pprofIO generates IO pprof-like profile (time spent in IO wait).
func pprofIO(w io.Writer) error {
	events, err := parseEvents()
	if err != nil {
		return err
	}
	prof := make(map[uint64]Record)
	for _, ev := range events {
		if ev.Type != trace.EvGoBlockNet || ev.Link == nil || ev.StkID == 0 || len(ev.Stk) == 0 {
			continue
		}
		rec := prof[ev.StkID]
		rec.stk = ev.Stk
		rec.n++
		rec.time += ev.Link.Ts - ev.Ts
		prof[ev.StkID] = rec
	}
	return buildProfile(prof).Write(w)
}

// pprofBlock generates blocking pprof-like profile (time spent blocked on synchronization primitives).
func pprofBlock(w io.Writer) error {
	events, err := parseEvents()
	if err != nil {
		return err
	}
	prof := make(map[uint64]Record)
	for _, ev := range events {
		switch ev.Type {
		case trace.EvGoBlockSend, trace.EvGoBlockRecv, trace.EvGoBlockSelect,
			trace.EvGoBlockSync, trace.EvGoBlockCond:
		default:
			continue
		}
		if ev.Link == nil || ev.StkID == 0 || len(ev.Stk) == 0 {
			continue
		}
		rec := prof[ev.StkID]
		rec.stk = ev.Stk
		rec.n++
		rec.time += ev.Link.Ts - ev.Ts
		prof[ev.StkID] = rec
	}
	return buildProfile(prof).Write(w)
}

// pprofSyscall generates syscall pprof-like profile (time spent blocked in syscalls).
func pprofSyscall(w io.Writer) error {
	events, err := parseEvents()
	if err != nil {
		return err
	}
	prof := make(map[uint64]Record)
	for _, ev := range events {
		if ev.Type != trace.EvGoSysCall || ev.Link == nil || ev.StkID == 0 || len(ev.Stk) == 0 {
			continue
		}
		rec := prof[ev.StkID]
		rec.stk = ev.Stk
		rec.n++
		rec.time += ev.Link.Ts - ev.Ts
		prof[ev.StkID] = rec
	}
	return buildProfile(prof).Write(w)
}

// pprofSched generates scheduler latency pprof-like profile
// (time between a goroutine become runnable and actually scheduled for execution).
func pprofSched(w io.Writer) error {
	events, err := parseEvents()
	if err != nil {
		return err
	}
	prof := make(map[uint64]Record)
	for _, ev := range events {
		if (ev.Type != trace.EvGoUnblock && ev.Type != trace.EvGoCreate) ||
			ev.Link == nil || ev.StkID == 0 || len(ev.Stk) == 0 {
			continue
		}
		rec := prof[ev.StkID]
		rec.stk = ev.Stk
		rec.n++
		rec.time += ev.Link.Ts - ev.Ts
		prof[ev.StkID] = rec
	}
	return buildProfile(prof).Write(w)
}

// serveSVGProfile serves pprof-like profile generated by prof as svg.
func serveSVGProfile(prof func(w io.Writer) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		blockf, err := ioutil.TempFile("", "block")
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to create temp file: %v", err), http.StatusInternalServerError)
			return
		}
		defer func() {
			blockf.Close()
			os.Remove(blockf.Name())
		}()
		blockb := bufio.NewWriter(blockf)
		if err := prof(blockb); err != nil {
			http.Error(w, fmt.Sprintf("failed to generate profile: %v", err), http.StatusInternalServerError)
			return
		}
		if err := blockb.Flush(); err != nil {
			http.Error(w, fmt.Sprintf("failed to flush temp file: %v", err), http.StatusInternalServerError)
			return
		}
		if err := blockf.Close(); err != nil {
			http.Error(w, fmt.Sprintf("failed to close temp file: %v", err), http.StatusInternalServerError)
			return
		}
		svgFilename := blockf.Name() + ".svg"
		if output, err := exec.Command("go", "tool", "pprof", "-svg", "-output", svgFilename, blockf.Name()).CombinedOutput(); err != nil {
			http.Error(w, fmt.Sprintf("failed to execute go tool pprof: %v\n%s", err, output), http.StatusInternalServerError)
			return
		}
		defer os.Remove(svgFilename)
		w.Header().Set("Content-Type", "image/svg+xml")
		http.ServeFile(w, r, svgFilename)
	}
}

func buildProfile(prof map[uint64]Record) *profile.Profile {
	p := &profile.Profile{
		PeriodType: &profile.ValueType{Type: "trace", Unit: "count"},
		Period:     1,
		SampleType: []*profile.ValueType{
			{Type: "contentions", Unit: "count"},
			{Type: "delay", Unit: "nanoseconds"},
		},
	}
	locs := make(map[uint64]*profile.Location)
	funcs := make(map[string]*profile.Function)
	for _, rec := range prof {
		var sloc []*profile.Location
		for _, frame := range rec.stk {
			loc := locs[frame.PC]
			if loc == nil {
				fn := funcs[frame.File+frame.Fn]
				if fn == nil {
					fn = &profile.Function{
						ID:         uint64(len(p.Function) + 1),
						Name:       frame.Fn,
						SystemName: frame.Fn,
						Filename:   frame.File,
					}
					p.Function = append(p.Function, fn)
					funcs[frame.File+frame.Fn] = fn
				}
				loc = &profile.Location{
					ID:      uint64(len(p.Location) + 1),
					Address: frame.PC,
					Line: []profile.Line{
						profile.Line{
							Function: fn,
							Line:     int64(frame.Line),
						},
					},
				}
				p.Location = append(p.Location, loc)
				locs[frame.PC] = loc
			}
			sloc = append(sloc, loc)
		}
		p.Sample = append(p.Sample, &profile.Sample{
			Value:    []int64{int64(rec.n), rec.time},
			Location: sloc,
		})
	}
	return p
}
