// Stuff related to user interface.

package top

import (
	"context"
	"fmt"
	"github.com/jroimartin/gocui"
	"github.com/lesovsky/pgcenter/internal/postgres"
	"github.com/lesovsky/pgcenter/internal/stat"
	"sync"
	"time"
)

const (
	msgPgStatStatementsUnavailable = "NOTICE: pg_stat_statements is not available in this database"
)

var (
	errSaved error       // keep error during program lifecycle
	cmdTimer *time.Timer // show cmdline's messages until timer is not expired
)

// Create and run UI. /* DEPRECATED */
func uiLoop(app *app) error {
	var e ErrorRate
	var errInterval = 1 * time.Second
	var errMaxcount = 5
	var wg sync.WaitGroup

	for {
		// init UI
		g, err := gocui.NewGui(gocui.OutputNormal)
		if err != nil {
			return fmt.Errorf("FATAL: gui creating failed with %s.\n", err)
		}

		app.ui = g

		// construct UI
		app.ui.SetManagerFunc(layout(app))

		// setup key shortcuts and bindings
		if err := keybindings(app); err != nil {
			return fmt.Errorf("FATAL: %s.\n", err)
		}

		// initial read of stats
		doWork(app)

		// run stats loop, that reads and display stats
		wg.Add(1)
		go statLoop(app, &wg)

		// run UI loop, that handle UI events
		if err := g.MainLoop(); err != nil && err != gocui.ErrQuit {
			// check errors rate and quit if them too much - allow no more than 5 errors within 1 second
			if err := e.Check(errInterval, errMaxcount); err != nil {
				return fmt.Errorf("FATAL: gui main loop failed with %s (%d errors within %.0f seconds).", err, e.errCnt, e.timeElapsed.Seconds())
			}
			// ignore errors if they are seldom - just rebuild UI
		}

		wg.Wait()
	}
}

//
func mainLoop(ctx context.Context, app *app) error {
	// init UI
	g, err := gocui.NewGui(gocui.OutputNormal)
	if err != nil {
		return fmt.Errorf("FATAL: gui creating failed with %s.\n", err)
	}

	app.ui = g

	// construct UI
	app.ui.SetManagerFunc(layout(app))

	// setup key shortcuts and bindings
	if err := keybindings(app); err != nil {
		return fmt.Errorf("FATAL: %s.\n", err)
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		doWork2(ctx, app)
		wg.Done()
	}()

	if err := g.MainLoop(); err != nil && err != gocui.ErrQuit {
		return err
	}

	wg.Wait()
	return nil
}

func doWork2(ctx context.Context, app *app) {
	// initial read of stats
	var wg sync.WaitGroup
	ch := make(chan stat.Stat2)

	wg.Add(1)
	go func() {
		collectStat(ctx, ch, app.db)
		close(ch)
		wg.Done()
	}()

	for {
		select {
		case s := <-ch:
			printStat(app, s)
		case <-ctx.Done():
			close(ch)
			wg.Wait()
			return
		}
	}
}

func collectStat(ctx context.Context, ch chan<- stat.Stat2, db *postgres.DB) {
	c, err := stat.NewCollector(db)
	if err != nil {
		fmt.Println(err)
		return
	}

	var i int
	for {
		stats, err := c.Update(db)
		if err != nil {
			fmt.Println(err)
		} else {
			ch <- stats
		}

		ticker := time.NewTicker(1 * time.Second)
		select {
		case <-ctx.Done():
			ticker.Stop()
			return
		case <-ticker.C:
			i++
			continue
		}
	}
}

func printStat(app *app, s stat.Stat2) {
	app.ui.Update(func(g *gocui.Gui) error {
		v, err := g.View("sysstat")
		if err != nil {
			return fmt.Errorf("Set focus on sysstat view failed: %s", err)
		}
		v.Clear()
		printSysstat2(v, s)

		v, err = g.View("pgstat")
		if err != nil {
			return fmt.Errorf("Set focus on pgstat view failed: %s", err)
		}
		v.Clear()
		printPgstat2(v, s)

		v, err = g.View("dbstat")
		if err != nil {
			return fmt.Errorf("Set focus on dbstat view failed: %s", err)
		}
		v.Clear()
		printDbstat2(v, app, s)

		//if app.config.aux > auxNone {
		//  v, err := g.View("aux")
		//  if err != nil {
		//    return fmt.Errorf("Set focus on aux view failed: %s", err)
		//  }
		//
		//  switch app.config.aux {
		//  case auxDiskstat:
		//    v.Clear()
		//    printIostat(v, app.stats.DiffDiskstats)
		//  case auxNicstat:
		//    v.Clear()
		//    printNicstat(v, app.stats.DiffNetdevs)
		//  case auxLogtail:
		//    // don't clear screen
		//    printLogtail(g, v)
		//  }
		//}
		return nil
	})
}

// END of new code **********************************************************

// Defines UI layout - views and their location.
func layout(app *app) func(g *gocui.Gui) error {
	return func(g *gocui.Gui) error {
		maxX, maxY := app.ui.Size()

		// Sysstat view
		if v, err := app.ui.SetView("sysstat", -1, -1, maxX-1/2, 4); err != nil {
			if err != gocui.ErrUnknownView {
				return fmt.Errorf("set sysstat view on layout failed: %s", err)
			}
			if _, err := app.ui.SetCurrentView("sysstat"); err != nil {
				return fmt.Errorf("set sysstat view as current on layout failed: %s", err)
			}
			v.Frame = false
		}

		// Postgres activity view
		if v, err := app.ui.SetView("pgstat", maxX/2, -1, maxX-1, 4); err != nil {
			if err != gocui.ErrUnknownView {
				return fmt.Errorf("set pgstat view on layout failed: %s", err)
			}
			v.Frame = false
		}

		// Command line
		if v, err := app.ui.SetView("cmdline", -1, 3, maxX-1, 5); err != nil {
			if err != gocui.ErrUnknownView {
				return fmt.Errorf("set cmdline view on layout failed: %s", err)
			}
			// show saved error to user if any
			if errSaved != nil {
				printCmdline(app.ui, "%s", errSaved)
				errSaved = nil
			}
			v.Frame = false
		}

		// Postgres' stats view
		if v, err := app.ui.SetView("dbstat", -1, 4, maxX-1, maxY-1); err != nil {
			if err != gocui.ErrUnknownView {
				return fmt.Errorf("set dbstat view on layout failed: %s", err)
			}
			v.Frame = false
		}

		// Aux stats view
		if app.config.aux > auxNone {
			if v, err := app.ui.SetView("aux", -1, 3*maxY/5-1, maxX-1, maxY-1); err != nil {
				if err != gocui.ErrUnknownView {
					return fmt.Errorf("set aux view on layout failed: %s", err)
				}
				fmt.Fprintln(v, "")
				v.Frame = false
			}
		}

		return nil
	}
}

// Wrapper function for printing messages in cmdline.
func printCmdline(g *gocui.Gui, format string, s ...interface{}) {
	g.Update(func(g *gocui.Gui) error {
		v, err := g.View("cmdline")
		if err != nil {
			return fmt.Errorf("Set focus on cmdline view failed: %s", err)
		}
		v.Clear()
		fmt.Fprintf(v, format, s...)

		// Clear the message after 1 second. Use timer here because it helps to show message a constant time and avoid blinking.
		if format != "" {
			// When user pushes buttons quickly and messages should be displayed a constant period of time, in that case
			// if there is a non-expired timer, refresh it (just stop existing and create new one)
			if cmdTimer != nil {
				cmdTimer.Stop()
			}
			cmdTimer = time.NewTimer(time.Second)
			go func() {
				<-cmdTimer.C
				printCmdline(g, "") // timer expired - wipe message.
			}()
		}

		return nil
	})
}
