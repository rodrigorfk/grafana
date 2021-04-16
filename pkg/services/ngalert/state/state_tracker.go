package state

import (
	"fmt"
	"sync"
	"time"

	"github.com/grafana/grafana/pkg/infra/log"

	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/grafana/pkg/services/ngalert/eval"
	ngModels "github.com/grafana/grafana/pkg/services/ngalert/models"
)

type AlertState struct {
	UID                string
	OrgID              int64
	CacheId            string
	Labels             data.Labels
	State              eval.State
	Results            []StateEvaluation
	StartsAt           time.Time
	EndsAt             time.Time
	LastEvaluationTime time.Time
	ProcessingTime     time.Duration
	Annotations        map[string]string
}

type StateEvaluation struct {
	EvaluationTime  time.Time
	EvaluationState eval.State
}

type cache struct {
	cacheMap map[string]AlertState
	mu       sync.Mutex
}

type StateTracker struct {
	stateCache cache
	quit       chan struct{}
	Log        log.Logger
}

func NewStateTracker(logger log.Logger) *StateTracker {
	tracker := &StateTracker{
		stateCache: cache{
			cacheMap: make(map[string]AlertState),
			mu:       sync.Mutex{},
		},
		quit: make(chan struct{}),
		Log:  logger,
	}
	go tracker.cleanUp()
	return tracker
}

func (st *StateTracker) getOrCreate(alertRule *ngModels.AlertRule, result eval.Result, processingTime time.Duration) AlertState {
	st.stateCache.mu.Lock()
	defer st.stateCache.mu.Unlock()

	// if duplicate labels exist, alertRule label will take precedence
	lbs := mergeLabels(alertRule.Labels, result.Instance)
	lbs["__alert_rule_uid__"] = alertRule.UID
	lbs["__alert_rule_namespace_uid__"] = alertRule.NamespaceUID
	lbs["__alert_rule_title__"] = alertRule.Title

	idString := fmt.Sprintf("%s", map[string]string(lbs))
	if state, ok := st.stateCache.cacheMap[idString]; ok {
		return state
	}

	annotations := map[string]string{}
	if len(alertRule.Annotations) > 0 {
		annotations = alertRule.Annotations
	}

	st.Log.Debug("adding new alert state cache entry", "cacheId", idString, "state", result.State.String(), "evaluatedAt", result.EvaluatedAt.String())
	newState := AlertState{
		UID:            alertRule.UID,
		OrgID:          alertRule.OrgID,
		CacheId:        idString,
		Labels:         lbs,
		State:          result.State,
		Results:        []StateEvaluation{},
		Annotations:    annotations,
		ProcessingTime: processingTime,
	}
	if result.State == eval.Alerting {
		newState.StartsAt = result.EvaluatedAt
	}
	st.stateCache.cacheMap[idString] = newState
	return newState
}

func (st *StateTracker) set(stateEntry AlertState) {
	st.stateCache.mu.Lock()
	defer st.stateCache.mu.Unlock()
	st.stateCache.cacheMap[stateEntry.CacheId] = stateEntry
}

func (st *StateTracker) Get(stateId string) AlertState {
	st.stateCache.mu.Lock()
	defer st.stateCache.mu.Unlock()
	return st.stateCache.cacheMap[stateId]
}

//Used to ensure a clean cache on startup
func (st *StateTracker) ResetCache() {
	st.stateCache.mu.Lock()
	defer st.stateCache.mu.Unlock()
	st.stateCache.cacheMap = make(map[string]AlertState)
}

func (st *StateTracker) ProcessEvalResults(alertRule *ngModels.AlertRule, results eval.Results, processingTime time.Duration) []AlertState {
	st.Log.Info("state tracker processing evaluation results", "uid", alertRule.UID, "resultCount", len(results))
	var changedStates []AlertState
	for _, result := range results {
		s := st.setNextState(alertRule, result, processingTime)
		changedStates = append(changedStates, s)
	}
	st.Log.Debug("returning changed states to scheduler", "count", len(changedStates))
	return changedStates
}

//TODO: When calculating if an alert should not be firing anymore, we should take three things into account:
// 1. The re-send the delay if any, we don't want to send every firing alert every time, we should have a fixed delay across all alerts to avoid saturating the notification system
// 2. The evaluation interval defined for this particular alert - we don't support that yet but will eventually allow you to define how often do you want this alert to be evaluted
// 3. The base interval defined by the scheduler - in the case where #2 is not yet an option we can use the base interval at which every alert runs.
//Set the current state based on evaluation results
func (st *StateTracker) setNextState(alertRule *ngModels.AlertRule, result eval.Result, processingTime time.Duration) AlertState {
	currentState := st.getOrCreate(alertRule, result, processingTime)
	st.Log.Debug("setting alert state", "uid", alertRule.UID)
	switch {
	case currentState.State == result.State:
		st.Log.Debug("no state transition", "cacheId", currentState.CacheId, "state", currentState.State.String())
		currentState.LastEvaluationTime = result.EvaluatedAt
		currentState.ProcessingTime = processingTime
		currentState.Results = append(currentState.Results, StateEvaluation{
			EvaluationTime:  result.EvaluatedAt,
			EvaluationState: result.State,
		})
		if currentState.State == eval.Alerting {
			currentState.EndsAt = result.EvaluatedAt.Add(alertRule.For * time.Second)
		}
		st.set(currentState)
		return currentState
	case currentState.State == eval.Normal && result.State == eval.Alerting:
		st.Log.Debug("state transition from normal to alerting", "cacheId", currentState.CacheId)
		currentState.State = eval.Alerting
		currentState.LastEvaluationTime = result.EvaluatedAt
		currentState.StartsAt = result.EvaluatedAt
		currentState.EndsAt = result.EvaluatedAt.Add(alertRule.For * time.Second)
		currentState.ProcessingTime = processingTime
		currentState.Results = append(currentState.Results, StateEvaluation{
			EvaluationTime:  result.EvaluatedAt,
			EvaluationState: result.State,
		})
		currentState.Annotations["alerting"] = result.EvaluatedAt.String()
		st.set(currentState)
		return currentState
	case currentState.State == eval.Alerting && result.State == eval.Normal:
		st.Log.Debug("state transition from alerting to normal", "cacheId", currentState.CacheId)
		currentState.State = eval.Normal
		currentState.LastEvaluationTime = result.EvaluatedAt
		currentState.EndsAt = result.EvaluatedAt
		currentState.ProcessingTime = processingTime
		currentState.Results = append(currentState.Results, StateEvaluation{
			EvaluationTime:  result.EvaluatedAt,
			EvaluationState: result.State,
		})
		st.set(currentState)
		return currentState
	default:
		return currentState
	}
}

func (st *StateTracker) GetAll() []AlertState {
	var states []AlertState
	st.stateCache.mu.Lock()
	defer st.stateCache.mu.Unlock()
	for _, v := range st.stateCache.cacheMap {
		states = append(states, v)
	}
	return states
}

func (st *StateTracker) cleanUp() {
	ticker := time.NewTicker(time.Duration(60) * time.Minute)
	st.Log.Debug("starting cleanup process", "intervalMinutes", 60)
	for {
		select {
		case <-ticker.C:
			st.trim()
		case <-st.quit:
			st.Log.Debug("stopping cleanup process", "now", time.Now())
			ticker.Stop()
			return
		}
	}
}

func (st *StateTracker) trim() {
	st.Log.Info("trimming alert state cache", "now", time.Now())
	st.stateCache.mu.Lock()
	defer st.stateCache.mu.Unlock()
	for _, v := range st.stateCache.cacheMap {
		if len(v.Results) > 100 {
			st.Log.Debug("trimming result set", "cacheId", v.CacheId, "count", len(v.Results)-100)
			newResults := make([]StateEvaluation, 100)
			copy(newResults, v.Results[100:])
			v.Results = newResults
			st.set(v)
		}
	}
}

func (a AlertState) Equals(b AlertState) bool {
	return a.UID == b.UID &&
		a.OrgID == b.OrgID &&
		a.CacheId == b.CacheId &&
		a.Labels.String() == b.Labels.String() &&
		a.State.String() == b.State.String() &&
		a.StartsAt == b.StartsAt &&
		a.EndsAt == b.EndsAt &&
		a.LastEvaluationTime == b.LastEvaluationTime
}

func (st *StateTracker) Put(states []AlertState) {
	for _, s := range states {
		st.set(s)
	}
}

// if duplicate labels exist, keep the value from the first set
func mergeLabels(a, b data.Labels) data.Labels {
	newLbs := data.Labels{}
	for k, v := range a {
		newLbs[k] = v
	}
	for k, v := range b {
		if _, ok := newLbs[k]; !ok {
			newLbs[k] = v
		}
	}
	return newLbs
}
