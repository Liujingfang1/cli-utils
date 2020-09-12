// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package apply

import (
	"fmt"

	"github.com/go-errors/errors"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/kubectl/pkg/cmd/apply"
	"sigs.k8s.io/cli-utils/pkg/apply/event"
	"sigs.k8s.io/cli-utils/pkg/apply/prune"
	"sigs.k8s.io/cli-utils/pkg/common"
	"sigs.k8s.io/cli-utils/pkg/inventory"
	"sigs.k8s.io/cli-utils/pkg/provider"
)

// NewDestroyer returns a new destroyer. It will set up the ApplyOptions and
// PruneOptions which are responsible for capturing any command line flags.
// It currently requires IOStreams, but this is a legacy from when
// the ApplyOptions were responsible for printing progress. This is now
// handled by a separate printer with the KubectlPrinterAdapter bridging
// between the two.
func NewDestroyer(provider provider.Provider, ioStreams genericclioptions.IOStreams) *Destroyer {
	return &Destroyer{
		ApplyOptions: apply.NewApplyOptions(ioStreams),
		// Create and maintain an empty set of UID's. This empty UID set
		// is used during prune calculation to prune every object.
		PruneOptions: prune.NewPruneOptions(sets.NewString()),
		provider:     provider,
		ioStreams:    ioStreams,
	}
}

// Destroyer performs the step of grabbing all the previous inventory objects and
// prune them. This also deletes all the previous inventory objects
type Destroyer struct {
	provider       provider.Provider
	ioStreams      genericclioptions.IOStreams
	ApplyOptions   *apply.ApplyOptions
	PruneOptions   *prune.PruneOptions
	invClient      inventory.InventoryClient
	DryRunStrategy common.DryRunStrategy
}

// Initialize sets up the Destroyer for actually doing an destroy against
// a cluster. This involves validating command line inputs and configuring
// clients for communicating with the cluster.
func (d *Destroyer) Initialize(cmd *cobra.Command, paths []string) error {
	fileNameFlags, err := common.DemandOneDirectory(paths)
	if err != nil {
		return err
	}
	d.ApplyOptions.DeleteFlags.FileNameFlags = &fileNameFlags
	err = d.ApplyOptions.Complete(d.provider.Factory(), cmd)
	if err != nil {
		return errors.WrapPrefix(err, "error setting up ApplyOptions", 1)
	}
	invClient, err := d.provider.InventoryClient()
	if err != nil {
		return errors.WrapPrefix(err, "error creating inventory client", 1)
	}
	d.invClient = invClient
	err = d.PruneOptions.Initialize(d.provider.Factory(), invClient)
	if err != nil {
		return errors.WrapPrefix(err, "error setting up PruneOptions", 1)
	}
	d.PruneOptions.Destroy = true

	// Propagate dry-run flags.
	d.ApplyOptions.DryRun = d.DryRunStrategy.ClientDryRun()
	d.ApplyOptions.ServerDryRun = d.DryRunStrategy.ServerDryRun()
	return nil
}

// Run performs the destroy step. This happens asynchronously
// on progress and any errors are reported back on the event channel.
func (d *Destroyer) Run() <-chan event.Event {
	ch := make(chan event.Event)

	go func() {
		defer close(ch)
		d.invClient.SetDryRunStrategy(d.DryRunStrategy)
		infos, err := d.ApplyOptions.GetObjects()
		if err != nil {
			ch <- event.Event{
				Type: event.ErrorType,
				ErrorEvent: event.ErrorEvent{
					Err: errors.WrapPrefix(err, "error reading resource manifests", 1),
				},
			}
			return
		}
		// Force a pruning of all cluster resources by clearing out the
		// local resources, and sending only the inventory object to the
		// prune.
		invInfo, _, err := inventory.SplitInfos(infos)
		if err != nil {
			ch <- event.Event{
				Type: event.ErrorType,
				ErrorEvent: event.ErrorEvent{
					Err: errors.WrapPrefix(err, "error clearing inventory object", 1),
				},
			}
			return
		}
		infos = []*resource.Info{invInfo}

		// Start the event transformer goroutine so we can transform
		// the Prune events emitted from the Prune function to Delete
		// Events. That we use Prune to implement destroy is an
		// implementation detail and the events should not be Prune events.
		tempChannel, completedChannel := runPruneEventTransformer(ch)
		err = d.PruneOptions.Prune(infos, tempChannel, prune.Options{
			DryRunStrategy:    d.DryRunStrategy,
			PropagationPolicy: metav1.DeletePropagationBackground,
		})
		// Now delete the inventory object as well.
		if invInfo != nil {
			_ = d.invClient.DeleteInventoryObj(invInfo)
		}

		// Close the tempChannel to signal to the event transformer that
		// it should terminate.
		close(tempChannel)
		// Wait for the event transformer to complete processing all
		// events and shut down before we continue.
		<-completedChannel
		if err != nil {
			// If we see an error here we just report it on the channel and then
			// give up. Eventually we might be able to determine which errors
			// are fatal and which might allow us to continue.
			ch <- event.Event{
				Type: event.ErrorType,
				ErrorEvent: event.ErrorEvent{
					Err: errors.WrapPrefix(err, "error pruning resources", 1),
				},
			}
			return
		}
		ch <- event.Event{
			Type: event.DeleteType,
			DeleteEvent: event.DeleteEvent{
				Type: event.DeleteEventCompleted,
			},
		}
	}()
	return ch
}

// SetFlags configures the command line flags needed for destroy
// This is a temporary solution as we should separate the configuration
// of cobra flags from the Destroyer.
func (d *Destroyer) SetFlags(cmd *cobra.Command) {
	d.ApplyOptions.DeleteFlags.AddFlags(cmd)
	for _, flag := range []string{"kustomize", "filename", "recursive"} {
		err := cmd.Flags().MarkHidden(flag)
		if err != nil {
			panic(err)
		}
	}
	d.ApplyOptions.RecordFlags.AddFlags(cmd)
	_ = cmd.Flags().MarkHidden("record")
	_ = cmd.Flags().MarkHidden("cascade")
	_ = cmd.Flags().MarkHidden("force")
	_ = cmd.Flags().MarkHidden("grace-period")
	_ = cmd.Flags().MarkHidden("timeout")
	_ = cmd.Flags().MarkHidden("wait")
	d.ApplyOptions.Overwrite = true
}

// runPruneEventTransformer creates a channel for events and
// starts a goroutine that will read from the channel until it
// is closed. All events will be republished as Delete events
// on the provided eventChannel. The function will also return
// a channel that it will close once the goroutine is shutting
// down.
func runPruneEventTransformer(eventChannel chan event.Event) (chan event.Event, <-chan struct{}) {
	completedChannel := make(chan struct{})
	tempEventChannel := make(chan event.Event)
	go func() {
		defer close(completedChannel)
		for msg := range tempEventChannel {
			eventChannel <- event.Event{
				Type: event.DeleteType,
				DeleteEvent: event.DeleteEvent{
					Type:      event.DeleteEventResourceUpdate,
					Operation: transformPruneOperation(msg.PruneEvent.Operation),
					Object:    msg.PruneEvent.Object,
				},
			}
		}
	}()
	return tempEventChannel, completedChannel
}

func transformPruneOperation(pruneOp event.PruneEventOperation) event.DeleteEventOperation {
	switch pruneOp {
	case event.PruneSkipped:
		return event.DeleteSkipped
	case event.Pruned:
		return event.Deleted
	default:
		panic(fmt.Errorf("unknown prune operation %s", pruneOp.String()))
	}
}
