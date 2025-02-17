/*
Package anser provides a document transformation and processing tool to
support data migrations.

Application

The anser.Application is the primary interface in which migrations are
defined and executed. Applications are constructed with a list of
MigrationGenerators, and relevant operations. Then the Setup method
configures the application, with an anser.Environment, which sets up
and collects dependency information. Finally, the Run method executes
the migrations in two phases: first by generating migration jobs, and
finally by running all migration jobs.

The ordering of migrations is derived from the dependency information
between generators and the jobs that they generate. When possible jobs
are executed in parallel, but the execution of migration operations is
a property of the queue object configured in the anser.Environment.

*/
package anser

import (
	"context"
	"time"

	"github.com/mongodb/amboy"
	"github.com/mongodb/anser/model"
	"github.com/mongodb/grip"
	"github.com/pkg/errors"
)

// Application define the root level of a database
// migration. Construct a migration application, pass in an
// anser.Environment object to the Setup function to initialize the
// application and then call Run to execute the application.
//
// Anser migrations run in two phases, a generation phase, which runs
// the jobs defined in the Generators field, and then runs all
// migration operations.
//
// The ordering of migrations is determined by the dependencies: there
// are dependencies between generator functions, and if a generator
// function has dependencies, then the migrations it produces will
// depend on all migrations produced by the generators dependencies.
//
// If the DryRun operation is set, then the application will run all
// of the migration.
//
// If the Limit operation is set to a value greater than 0, the
// application will only run *that* number of jobs.
type Application struct {
	Generators []Generator
	Options    model.ApplicationOptions
	env        Environment
	hasSetup   bool
}

// Setup takes a configured anser.Environment implementation and
// configures all generator.
//
// You can only run this function once; subsequent attempts return an
// error but are a noop otherwise.
func (a *Application) Setup(e Environment) error {
	if a.hasSetup {
		return errors.New("cannot setup an application more than once")
	}

	if e == nil {
		return errors.New("cannot setup an application with a nil environment")
	}

	a.env = e
	network, err := e.GetDependencyNetwork()
	if err != nil {
		return errors.Wrap(err, "getting dependency tracker")
	}

	for _, gen := range a.Generators {
		network.Add(gen.ID(), gen.Dependency().Edges())
	}

	a.hasSetup = true
	return nil
}

func (a *Application) Run(ctx context.Context) error {
	queue, err := a.env.GetQueue()
	if err != nil {
		return errors.Wrap(err, "getting queue")
	}

	catcher := grip.NewCatcher()
	// iterate through generators
	for _, generator := range a.Generators {
		catcher.Add(queue.Put(ctx, generator))
	}

	if catcher.HasErrors() {
		return errors.Wrap(catcher.Resolve(), "adding generation jobs")
	}

	amboy.WaitInterval(ctx, queue, time.Second)
	if ctx.Err() != nil {
		return errors.New("migration operation canceled")
	}

	numMigrations, err := addMigrationJobs(ctx, queue, a.Options.DryRun, a.Options.Limit)
	if err != nil {
		return errors.Wrap(err, "adding generated migration jobs")
	}

	if a.Options.DryRun {
		grip.Noticef("ending dry run, generated %d jobs in %d migrations", numMigrations, len(a.Generators))
		return nil
	}

	grip.Infof("added %d migration jobs from %d migrations", numMigrations, len(a.Generators))
	grip.Noticef("waiting for %d migration jobs of %d migrations", numMigrations, len(a.Generators))
	amboy.WaitInterval(ctx, queue, time.Second)
	if ctx.Err() != nil {
		return errors.New("migration operation canceled")
	}

	if err := amboy.ResolveErrors(ctx, queue); err != nil {
		return errors.Wrap(err, "running migration jobs")
	}

	return nil
}
