// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package apply

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/cli-utils/pkg/apply/event"
	"sigs.k8s.io/cli-utils/pkg/common"
	pollevent "sigs.k8s.io/cli-utils/pkg/kstatus/polling/event"
	"sigs.k8s.io/cli-utils/pkg/object"
)

// BasicPrinter is a simple implementation that just prints the events
// from the channel in the default format for kubectl.
// We need to support different printers for different output formats.
type BasicPrinter struct {
	IOStreams genericclioptions.IOStreams
}

type applyStats struct {
	serversideApplied int
	created           int
	unchanged         int
	configured        int
}

func (a *applyStats) inc(op event.ApplyEventOperation) {
	switch op {
	case event.ServersideApplied:
		a.serversideApplied++
	case event.Created:
		a.created++
	case event.Unchanged:
		a.unchanged++
	case event.Configured:
		a.configured++
	default:
		panic(fmt.Errorf("unknown apply operation %s", op.String()))
	}
}

func (a *applyStats) sum() int {
	return a.serversideApplied + a.configured + a.unchanged + a.created
}

type pruneStats struct {
	pruned  int
	skipped int
}

func (p *pruneStats) incPruned() {
	p.pruned++
}

func (p *pruneStats) incSkipped() {
	p.skipped++
}

type deleteStats struct {
	deleted int
	skipped int
}

func (d *deleteStats) incDeleted() {
	d.deleted++
}

func (d *deleteStats) incSkipped() {
	d.skipped++
}

type statusCollector struct {
	latestStatus map[object.ObjMetadata]pollevent.Event
	printStatus  bool
}

func (sc *statusCollector) updateStatus(id object.ObjMetadata, se pollevent.Event) {
	sc.latestStatus[id] = se
}

// Print outputs the events from the provided channel in a simple
// format on StdOut. As we support other printer implementations
// this should probably be an interface.
// This function will block until the channel is closed.
func (b *BasicPrinter) Print(ch <-chan event.Event, previewStrategy common.DryRunStrategy) error {
	printFunc := b.getPrintFunc(previewStrategy)
	applyStats := &applyStats{}
	statusCollector := &statusCollector{
		latestStatus: make(map[object.ObjMetadata]pollevent.Event),
		printStatus:  false,
	}
	pruneStats := &pruneStats{}
	deleteStats := &deleteStats{}
	for e := range ch {
		switch e.Type {
		case event.ErrorType:
			return e.ErrorEvent.Err
		case event.ApplyType:
			b.processApplyEvent(e.ApplyEvent, applyStats, statusCollector, printFunc)
		case event.StatusType:
			b.processStatusEvent(e.StatusEvent, statusCollector, printFunc)
		case event.PruneType:
			b.processPruneEvent(e.PruneEvent, pruneStats, printFunc)
		case event.DeleteType:
			b.processDeleteEvent(e.DeleteEvent, deleteStats, printFunc)
		}
	}
	return nil
}

func (b *BasicPrinter) processApplyEvent(ae event.ApplyEvent, as *applyStats,
	c *statusCollector, p printFunc) {
	switch ae.Type {
	case event.ApplyEventCompleted:
		output := fmt.Sprintf("%d resource(s) applied. %d created, %d unchanged, %d configured",
			as.sum(), as.created, as.unchanged, as.configured)
		// Only print information about serverside apply if some of the
		// resources actually were applied serverside.
		if as.serversideApplied > 0 {
			output += fmt.Sprintf(", %d serverside applied", as.serversideApplied)
		}
		p(output)
		c.printStatus = true
		for id, se := range c.latestStatus {
			printResourceStatus(id, se, p)
		}
	case event.ApplyEventResourceUpdate:
		obj := ae.Object
		gvk := obj.GetObjectKind().GroupVersionKind()
		name := getName(obj)
		as.inc(ae.Operation)
		p("%s %s", resourceIDToString(gvk.GroupKind(), name),
			strings.ToLower(ae.Operation.String()))
	}
}

func (b *BasicPrinter) processStatusEvent(se pollevent.Event, sc *statusCollector, p printFunc) {
	switch se.EventType {
	case pollevent.ResourceUpdateEvent:
		id := se.Resource.Identifier
		sc.updateStatus(id, se)
		if sc.printStatus {
			printResourceStatus(id, se, p)
		}
	case pollevent.ErrorEvent:
		id := se.Resource.Identifier
		gk := id.GroupKind
		p("%s error: %s\n", resourceIDToString(gk, id.Name),
			se.Error.Error())
	case pollevent.CompletedEvent:
		sc.printStatus = false
		p("all resources has reached the Current status")
	case pollevent.AbortedEvent:
		sc.printStatus = false
		p("resources failed to the reached Current status")
	}
}

func printResourceStatus(id object.ObjMetadata, se pollevent.Event, p printFunc) {
	p("%s is %s: %s", resourceIDToString(id.GroupKind, id.Name),
		se.Resource.Status.String(), se.Resource.Message)
}

func (b *BasicPrinter) processPruneEvent(pe event.PruneEvent, ps *pruneStats, p printFunc) {
	switch pe.Type {
	case event.PruneEventCompleted:
		p("%d resource(s) pruned, %d skipped", ps.pruned, ps.skipped)
	case event.PruneEventResourceUpdate:
		obj := pe.Object
		gvk := obj.GetObjectKind().GroupVersionKind()
		name := getName(obj)
		switch pe.Operation {
		case event.Pruned:
			ps.incPruned()
			p("%s %s", resourceIDToString(gvk.GroupKind(), name), "pruned")
		case event.PruneSkipped:
			ps.incSkipped()
			p("%s %s", resourceIDToString(gvk.GroupKind(), name), "prune skipped")
		}
	}
}

func (b *BasicPrinter) processDeleteEvent(de event.DeleteEvent, ds *deleteStats, p printFunc) {
	switch de.Type {
	case event.DeleteEventCompleted:
		p("%d resource(s) deleted, %d skipped", ds.deleted, ds.skipped)
	case event.DeleteEventResourceUpdate:
		obj := de.Object
		gvk := obj.GetObjectKind().GroupVersionKind()
		name := getName(obj)
		switch de.Operation {
		case event.Deleted:
			ds.incDeleted()
			p("%s %s", resourceIDToString(gvk.GroupKind(), name), "deleted")
		case event.DeleteSkipped:
			ds.incSkipped()
			p("%s %s", resourceIDToString(gvk.GroupKind(), name), "delete skipped")
		}
	}
}

func getName(obj runtime.Object) string {
	if acc, err := meta.Accessor(obj); err == nil {
		if n := acc.GetName(); len(n) > 0 {
			return n
		}
	}
	return "<unknown>"
}

// resourceIDToString returns the string representation of a GroupKind and a resource name.
func resourceIDToString(gk schema.GroupKind, name string) string {
	return fmt.Sprintf("%s/%s", strings.ToLower(gk.String()), name)
}

type printFunc func(format string, a ...interface{})

func (b *BasicPrinter) getPrintFunc(previewStrategy common.DryRunStrategy) printFunc {
	return func(format string, a ...interface{}) {
		if previewStrategy.ClientDryRun() {
			format += " (preview)"
		} else if previewStrategy.ServerDryRun() {
			format += " (preview-server)"
		}
		fmt.Fprintf(b.IOStreams.Out, format+"\n", a...)
	}
}
