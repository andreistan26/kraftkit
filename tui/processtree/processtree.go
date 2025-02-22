// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2022, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.
package processtree

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/LastPossum/kamino"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/stopwatch"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"
	"golang.org/x/term"

	"kraftkit.sh/iostreams"
	"kraftkit.sh/log"
)

type (
	SpinnerProcess func(ctx context.Context) error
	processExitMsg *ProcessTreeItem
)

type SpinnerProcessStatus uint

const (
	StatusPending SpinnerProcessStatus = iota
	StatusRunning
	StatusRunningChild
	StatusRunningButAChildHasFailed
	StatusFailed
	StatusFailedChild
	StatusSuccess
)

const (
	INDENTS = 4
	LOGLEN  = 5
)

var tprog *tea.Program

type ProcessTreeItem struct {
	textLeft  string
	textRight string
	status    SpinnerProcessStatus
	spinner   spinner.Model
	children  []*ProcessTreeItem
	logs      []string
	logChan   chan *ProcessTreeItem
	process   SpinnerProcess
	timer     stopwatch.Model
	norender  bool
	ctx       context.Context
	timeout   time.Duration
	err       error
	ellipsis  string
	hideError bool
}

type ProcessTree struct {
	verb      string
	channel   chan *ProcessTreeItem
	tree      []*ProcessTreeItem
	quitting  bool
	ctx       context.Context
	timer     stopwatch.Model
	width     int
	rightPad  int
	parallel  bool
	norender  bool
	finished  int
	total     int
	err       error
	errChan   chan error
	failFast  bool
	oldOut    iostreams.FileWriter
	hide      bool
	hideError bool
	timeout   time.Duration
}

func NewProcessTree(ctx context.Context, opts []ProcessTreeOption, tree ...*ProcessTreeItem) (*ProcessTree, error) {
	if len(tree) == 0 {
		return nil, fmt.Errorf("cannot instantiate process tree without sub processes")
	}

	pt := &ProcessTree{
		tree:      tree,
		ctx:       ctx,
		timer:     stopwatch.NewWithInterval(time.Millisecond * 100),
		channel:   make(chan *ProcessTreeItem),
		errChan:   make(chan error),
		finished:  0,
		oldOut:    iostreams.G(ctx).Out,
		hideError: false,
	}

	for _, opt := range opts {
		if err := opt(pt); err != nil {
			return nil, err
		}
	}

	total := 0

	_ = pt.traverseTreeAndCall(tree, func(item *ProcessTreeItem) error {
		total++
		item.norender = pt.norender
		item.timeout = pt.timeout
		item.hideError = pt.hideError

		if pt.norender {
			item.ctx = pt.ctx
			return nil
		}

		ictx := pt.ctx

		logger, err := kamino.Clone(log.G(ictx),
			kamino.WithZeroUnexported(),
		)
		if err != nil {
			return err
		}

		logger.Out = item

		if formatter, ok := logger.Formatter.(*log.TextFormatter); ok {
			formatter.ForceColors = termenv.DefaultOutput().ColorProfile() != termenv.Ascii
			formatter.ForceFormatting = true
			logger.Formatter = formatter
		}

		ictx = log.WithLogger(ictx, logger)

		ios, err := kamino.Clone(iostreams.G(ictx),
			kamino.WithZeroUnexported(),
		)
		if err != nil {
			return err
		}

		ios.Out = iostreams.NewNoTTYWriter(item, iostreams.G(ctx).Out.Fd())
		ios.ErrOut = item
		ios.In = iostreams.G(ctx).In
		ictx = iostreams.WithIOStreams(ictx, ios)

		item.ctx = ictx

		return nil
	})

	pt.total = total

	return pt, nil
}

func NewProcessTreeItem(textLeft, textRight string, process SpinnerProcess, children ...*ProcessTreeItem) *ProcessTreeItem {
	return &ProcessTreeItem{
		textLeft:  textLeft,
		textRight: textRight,
		process:   process,
		status:    StatusPending,
		children:  children,
		timer:     stopwatch.NewWithInterval(time.Millisecond * 100),
		logChan:   make(chan *ProcessTreeItem),
		spinner:   spinner.New(),
	}
}

// Write implements `io.Writer` so we can correctly direct the output from tree
// process to an inline fancy logger
func (pti *ProcessTreeItem) Write(p []byte) (int, error) {
	// Remove the last line which is usually appended by a logger
	line := strings.TrimSuffix(string(p), "\n")

	// Split all lines up so we can individually append them
	lines := strings.Split(strings.ReplaceAll(line, "\r\n", "\n"), "\n")

	pti.logs = append(pti.logs, lines...)

	return len(p), nil
}

func (pti *ProcessTreeItem) Fd() int {
	return 0
}

func (pti *ProcessTreeItem) Close() error {
	return nil
}

func (pt *ProcessTree) Start() error {
	teaOpts := []tea.ProgramOption{
		tea.WithContext(pt.ctx),
	}

	if iostreams.G(pt.ctx).IsStdinTTY() {
		teaOpts = append(teaOpts, tea.WithInput(iostreams.G(pt.ctx).In))
	} else {
		teaOpts = append(teaOpts, tea.WithInput(nil))
	}

	// Restore the old output for the IOStreams which is manipulated per process.
	defer func() {
		iostreams.G(pt.ctx).Out = pt.oldOut
		log.G(pt.ctx).Out = iostreams.G(pt.ctx).Out
	}()

	if pt.norender {
		teaOpts = append(teaOpts, tea.WithoutRenderer())
	} else {
		// Set this super early (even before bubbletea), as fast exiting processes
		// may not have received the window size update and therefore pt.width is
		// set to zero.
		pt.width, _, _ = term.GetSize(int(os.Stdout.Fd()))
	}

	tprog = tea.NewProgram(pt, teaOpts...)

	if _, err := tprog.Run(); err != nil {
		return err
	}

	return pt.err
}

func (pt *ProcessTree) Init() tea.Cmd {
	//nolint:staticcheck
	cmds := []tea.Cmd{
		waitForProcessExit(pt.channel),
		spinner.Tick,
		pt.timer.Init(),
	}

	// Start all child processes
	children := pt.getNextReadyChildren(pt.tree)
	for _, pti := range children {
		pti := pti
		pti.timeout = pt.timeout

		cmds = append(cmds, pt.waitForProcessCmd(pti))
		cmds = append(cmds, pti.timer.Init())
	}

	return tea.Batch(cmds...)
}

func (pt ProcessTree) getNextReadyChildren(tree []*ProcessTreeItem) []*ProcessTreeItem {
	var items []*ProcessTreeItem

	for _, item := range tree {
		var subprocesses []*ProcessTreeItem
		completed := 0
		failed := 0

		if len(item.children) > 0 {
			subprocesses = pt.getNextReadyChildren(item.children)

			// Add all subprocesses if in parallel mode
			if pt.parallel {
				items = append(items, subprocesses...)

				// We can only add 1 item if non-parallel and there are actual
			} else if len(subprocesses) > 0 {
				items = append(items, subprocesses[0])
			}

			// Determine the status of immediate children
			for _, child := range item.children {
				if child.status == StatusFailed ||
					child.status == StatusFailedChild {
					failed++
				} else if child.status == StatusSuccess {
					completed++
				}
			}
		}

		// Only start the parent process if all children have succeeded or if there
		// no children and the status is pending
		if len(subprocesses) == 0 &&
			failed == 0 &&
			(pt.parallel || (len(items) == 0 && !pt.parallel)) &&
			completed == len(item.children) &&
			(item.status == StatusPending || item.status == StatusRunningChild) {
			items = append(items, item)
		}
	}

	return items
}

func (pt *ProcessTree) traverseTreeAndCall(items []*ProcessTreeItem, callback func(*ProcessTreeItem) error) error {
	for _, child := range items {
		if len(child.children) > 0 {
			if err := pt.traverseTreeAndCall(child.children, callback); err != nil {
				return err
			}
		}

		// Call the callback on the leaf node first
		if err := callback(child); err != nil {
			return err
		}
	}

	return nil
}

func (pt *ProcessTree) waitForProcessCmd(item *ProcessTreeItem) tea.Cmd {
	return func() tea.Msg {
		item := item // golang closures

		if pt.norender {
			log.G(item.ctx).Info(item.textLeft)
		}

		// Set the process to running
		item.status = StatusRunning

		if err := item.process(item.ctx); err != nil {
			log.G(item.ctx).Error(err)
			item.status = StatusFailed
			pt.err = err
			if pt.failFast {
				pt.quitting = true
			}
		} else {
			item.status = StatusSuccess
		}

		pt.channel <- item

		return item.timer.Stop()
	}
}

func waitForProcessExit(sub chan *ProcessTreeItem) tea.Cmd {
	return func() tea.Msg {
		return processExitMsg(<-sub)
	}
}
