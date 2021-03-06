package orapub

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/xtracdev/goes"
	"github.com/xtracdev/oraconn"
)

const consecutiveErrorsThreshold = 100

//EventProcessor is implemented for hooking into the processing of the events
//in the event store publish table
type EventProcessor struct {
	Initialize func(*sql.DB) error
	Processor  func(db *sql.DB, e *goes.Event) error
}

//EventSpec is the specification of a published event
type EventSpec struct {
	AggregateId string
	Version     int
}

//Event processors are registered at the package level
var eventProcessors map[string]EventProcessor

var ErrNoEventProcessorsRegistered = errors.New("No event processors registered - exiting event processing loop")
var ErrNotConnected = errors.New("Not connected to database - call connect first")
var ErrNilEventProcessorField = errors.New("Registered event processor with one or more nil fields.")

func init() {
	eventProcessors = make(map[string]EventProcessor)
}

//OraPub provides the ability to process the events in the event store publish table.
type OraPub struct {
	db            *oraconn.OracleDB
	LoopExitError error
}

//ClearRegisteredEventProcessors clears out the registered event processors. This is useful when testing.
func ClearRegisteredEventProcessors() {
	eventProcessors = make(map[string]EventProcessor)
}

//RegisterEventProcessor registers an event processor with OraPub. Event processors registered
//with OraPub are initialized then receive events when processing the events in the event table.
func RegisterEventProcessor(name string, eventProcessor EventProcessor) error {
	if eventProcessor.Processor == nil || eventProcessor.Initialize == nil {
		return ErrNilEventProcessorField
	}
	eventProcessors[name] = eventProcessor

	return nil
}

func (op *OraPub) extractDB() *sql.DB {
	//Grab the database connection to pass to the initialization and event processing
	//handlers. A nil database connection makes sense for unit testing.
	var db *sql.DB
	if op.db != nil {
		db = op.db.DB
	} else {
		log.Warn("No database connection for InitializeProcessors - this only makes sense for unit testing")
	}
	return db
}

//InitializeProcessors initializes all the processors for registered event handlers.
func (op *OraPub) InitializeProcessors() error {

	db := op.extractDB()
	for k, p := range eventProcessors {
		log.Infof("Initializing %s", k)
		err := p.Initialize(db)
		if err != nil {
			return err
		}
	}

	return nil
}

//processEvent invokes the Processor method with the given event on every EventProcessor
//registered with OraPub
func (op *OraPub) processEvent(event *goes.Event) {
	db := op.extractDB()
	for _, p := range eventProcessors {
		err := p.Processor(db, event)
		if err != nil {
			log.Warnf("Error processing event %v: %s", event, err.Error())
		}
	}
}

//Connect creates the database connection with the given connection string and
//max connection retrys.
func (op *OraPub) Connect(connectStr string, maxTrys int) error {
	db, err := oraconn.OpenAndConnect(connectStr, maxTrys)
	if err != nil {
		log.Warnf("Error connecting to oracle: %s", err.Error())
		return err
	}

	op.db = db

	return nil
}

//handleConnectionError determines if the given error is a connection error, and if so,
//attempts to reconnect to the database. True is returned when the error indicates a connection
//error, and the reconnect is successful.
func (op *OraPub) handleConnectionError(err error) bool {
	if oraconn.IsConnectionError(err) {
		err := op.db.Reconnect(5)
		return err == nil
	}

	return false
}

//pollEvents polls the publish table for events that have been published and are available for processing.
//Note the use of select for update - this is the mechanism that allows multiple OraPub instances to be
//active concurrently.
func (op *OraPub) pollEvents(tx *sql.Tx) ([]EventSpec, error) {
	var eventSpecs []EventSpec

	if tx == nil {
		var makeTxErr error
		log.Warn("No TX provided to PollEvents - creating tx")
		tx, makeTxErr = op.db.Begin()
		if makeTxErr != nil {
			return nil, makeTxErr
		}
		defer tx.Rollback()
	}

	//Select a batch of events, but no more than 100
	rows, err := tx.Query(`select aggregate_id, version from t_aepb_publish where rownum < 101 order by version for update`)
	if err != nil {
		op.handleConnectionError(err)
		return nil, err
	}

	defer rows.Close()

	var version int
	var aggID string

	for rows.Next() {
		rows.Scan(&aggID, &version)
		es := EventSpec{
			AggregateId: aggID,
			Version:     version,
		}

		eventSpecs = append(eventSpecs, es)
	}

	err = rows.Err()
	if err != nil {
		op.handleConnectionError(err)
	}

	return eventSpecs, err
}

//deleteEvent removes a published event that have been processed, or have at least attempted to be
//processed.
func (op *OraPub) deleteEvent(tx *sql.Tx, es EventSpec) error {
	_, err := tx.Exec("delete from t_aepb_publish where aggregate_id = :1 and version = :2",
		es.AggregateId, es.Version)
	if err != nil {
		log.Warnf("Error deleting aggregate, version %s, %d: %s", es.AggregateId, es.Version, err.Error())
		op.handleConnectionError(err)
	}

	return err
}

//deleteProcessedEvents iterates through a list of event specs, deleting the associated event from the
//publish table.
func (op *OraPub) deleteProcessedEvents(specs []EventSpec) error {
	for _, es := range specs {
		_, err := op.db.Exec("delete from t_aepb_publish where aggregate_id = :1 and version = :2",
			es.AggregateId, es.Version)
		if err != nil {
			log.Warnf("Error deleting aggregate, version %s, %d: %s", es.AggregateId, es.Version, err.Error())
			op.handleConnectionError(err)
		}
	}

	return nil
}

func (op *OraPub) retrieveEventDetail(aggregateId string, version int) (*goes.Event, error) {
	row, err := op.db.Query("select typecode, payload from t_aeev_events where aggregate_id = :1 and version = :2",
		aggregateId, version)
	if err != nil {
		op.handleConnectionError(err)
		return nil, err
	}

	defer row.Close()

	var typecode string
	var payload []byte
	var scanned bool

	if row.Next() {
		row.Scan(&typecode, &payload)
		scanned = true
	}

	if !scanned {
		return nil, fmt.Errorf("Aggregate %s version %d not found", aggregateId, version)
	}

	err = row.Err()
	if err != nil {
		op.handleConnectionError(err)
		return nil, err
	}

	eventPtr := &goes.Event{
		Source:   aggregateId,
		Version:  version,
		TypeCode: typecode,
		Payload:  payload,
	}

	//log.Infof("Event read from db: %v", *eventPtr)

	return eventPtr, nil
}

//ProcessEvents processes the events in the publish table, sending each event to the registered
//event processors. Event processing is done within a transaction, which is used to isolate the processing
//of events amidst concurrent event processors. The transaction does not extend to the event processors - if they
//return errors they will not get another shot at processing the event. Also, if an error occurs causing the
//transaction to rollback, it is possible the event processor could be invoked with the same event at a later time.
func (op *OraPub) ProcessEvents(loop bool) {
	op.LoopExitError = nil

	var consecutiveErrors int

	//Don't process events if there are no handlers registered to process them
	if len(eventProcessors) == 0 {
		op.LoopExitError = ErrNoEventProcessorsRegistered
		return
	}

	//If we enter this module unconnected, we should try to connect
	if op.db == nil {
		op.LoopExitError = ErrNotConnected
		return
	}

	for {
		var loopErr error
		var eventSpecs []EventSpec

		log.Debug("start process events transaction")
		txn, loopErr := op.db.Begin()
		if loopErr != nil {
			log.Warn(loopErr.Error())
			goto exitpt
		}

		log.Debug("poll for events")
		eventSpecs, loopErr = op.pollEvents(txn)
		if loopErr != nil {
			log.Warn(loopErr.Error())
			goto exitpt
		}

		if len(eventSpecs) == 0 {
			log.Infof("Nothing to do... time for a 5 second sleep")
			txn.Rollback()
			time.Sleep(5 * time.Second)
			goto exitpt
		}

		log.Debug("process events")
		for _, eventContext := range eventSpecs {

			log.Debugf("process %s:%d", eventContext.AggregateId, eventContext.Version)
			e, loopErr := op.retrieveEventDetail(eventContext.AggregateId, eventContext.Version)
			if loopErr != nil {
				log.Warnf("Error reading event to process (%v): %s", eventContext, loopErr)
				goto exitpt
			}

			for p, processor := range eventProcessors {
				log.Debug("call processor")
				procErr := processor.Processor(op.db.DB, e)
				if procErr == nil {
					op.deleteEvent(txn, eventContext)
				} else {
					log.Warnf("%s: error processing event %v: %s", p, e, procErr.Error())
				}
			}

		}

		log.Debug("commit txn")
		txn.Commit()
		consecutiveErrors = 0

	exitpt:
		if loopErr != nil {
			consecutiveErrors += 1
			time.Sleep(1 * time.Second) //Error delay
			if txn != nil {
				txn.Rollback()
			}

			if op.handleConnectionError(loopErr) {
				consecutiveErrors = 0
			}

			if consecutiveErrors > consecutiveErrorsThreshold {
				op.LoopExitError = loopErr
				return
			}
		}

		if loop != true {
			break
		} else {
			continue
		}
	}
}

func (op *OraPub) IsHealth() bool {
	return op.LoopExitError == nil && op.isDbHealth()
}

func (op *OraPub) isDbHealth() bool {
	if db := op.extractDB(); db != nil {
		err := db.Ping()
		if err != nil {
			log.Info("Ping DB returns error: ", err)
		}
		return err == nil
	}
	return false
}
