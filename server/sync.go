// Copyright (C) 2015 NTT Innovation Institute, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwan/gohan/db"
	"github.com/cloudwan/gohan/db/pagination"
	"github.com/cloudwan/gohan/db/transaction"
	"github.com/cloudwan/gohan/extension"
	"github.com/cloudwan/gohan/job"

	"github.com/cloudwan/gohan/schema"
	gohan_sync "github.com/cloudwan/gohan/sync"
	"github.com/cloudwan/gohan/util"
)

const (
	syncPath = "gohan/cluster/sync"
	lockPath = "gohan/cluster/lock"

	configPrefix     = "/config/"
	statePrefix      = "/state/"
	monitoringPrefix = "/monitoring/"

	eventPollingTime  = 30 * time.Second
	eventPollingLimit = 10000

	StateUpdateEventName      = "state_update"
	MonitoringUpdateEventName = "monitoring_update"
)

var transactionCommited chan int

func transactionCommitInformer() chan int {
	if transactionCommited == nil {
		transactionCommited = make(chan int, 1)
	}
	return transactionCommited
}

//DbSyncWrapper wraps db.DB so it logs events in database on every transaction.
type DbSyncWrapper struct {
	db.DB
}

// Begin wraps transaction object with sync
func (sw *DbSyncWrapper) Begin() (transaction.Transaction, error) {
	tx, err := sw.DB.Begin()
	if err != nil {
		return nil, err
	}
	return syncTransactionWrap(tx), nil
}

type transactionEventLogger struct {
	transaction.Transaction
	eventLogged bool
}

func syncTransactionWrap(tx transaction.Transaction) *transactionEventLogger {
	return &transactionEventLogger{tx, false}
}

func (tl *transactionEventLogger) logEvent(eventType string, resource *schema.Resource, version int64) error {
	schemaManager := schema.GetManager()
	eventSchema, ok := schemaManager.Schema("event")
	if !ok {
		return fmt.Errorf("event schema not found")
	}

	if resource.Schema().Metadata["nosync"] == true {
		log.Debug("skipping event logging for schema: %s", resource.Schema().ID)
		return nil
	}
	body, err := resource.JSONString()
	if err != nil {
		return fmt.Errorf("Error during event resource deserialisation: %s", err.Error())
	}
	eventResource, err := schema.NewResource(eventSchema, map[string]interface{}{
		"type":      eventType,
		"path":      resource.Path(),
		"version":   version,
		"body":      body,
		"timestamp": int64(time.Now().Unix()),
	})
	tl.eventLogged = true
	return tl.Transaction.Create(eventResource)
}

func (tl *transactionEventLogger) Create(resource *schema.Resource) error {
	err := tl.Transaction.Create(resource)
	if err != nil {
		return err
	}
	return tl.logEvent("create", resource, 1)
}

func (tl *transactionEventLogger) Update(resource *schema.Resource) error {
	err := tl.Transaction.Update(resource)
	if err != nil {
		return err
	}
	if !resource.Schema().StateVersioning() {
		return tl.logEvent("update", resource, 0)
	}
	state, err := tl.StateFetch(resource.Schema(), transaction.IDFilter(resource.ID()))
	if err != nil {
		return err
	}
	return tl.logEvent("update", resource, state.ConfigVersion)
}

func (tl *transactionEventLogger) Delete(s *schema.Schema, resourceID interface{}) error {
	resource, err := tl.Fetch(s, transaction.IDFilter(resourceID))
	if err != nil {
		return err
	}
	configVersion := int64(0)
	if resource.Schema().StateVersioning() {
		state, err := tl.StateFetch(s, transaction.IDFilter(resourceID))
		if err != nil {
			return err
		}
		configVersion = state.ConfigVersion + 1
	}
	err = tl.Transaction.Delete(s, resourceID)
	if err != nil {
		return err
	}
	return tl.logEvent("delete", resource, configVersion)
}

func (tl *transactionEventLogger) Commit() error {
	err := tl.Transaction.Commit()
	if err != nil {
		return err
	}
	if !tl.eventLogged {
		return nil
	}
	committed := transactionCommitInformer()
	select {
	case committed <- 1:
	default:
	}
	return nil
}

func (server *Server) listEvents() ([]*schema.Resource, error) {
	tx, err := server.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Close()
	schemaManager := schema.GetManager()
	eventSchema, _ := schemaManager.Schema("event")
	paginator, _ := pagination.NewPaginator(eventSchema, "id", pagination.ASC, eventPollingLimit, 0)
	resourceList, _, err := tx.List(eventSchema, nil, paginator)
	if err != nil {
		return nil, err
	}
	return resourceList, nil
}

func (server *Server) syncEvent(resource *schema.Resource) error {
	schemaManager := schema.GetManager()
	eventSchema, _ := schemaManager.Schema("event")
	tx, err := server.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Close()
	eventType := resource.Get("type").(string)
	resourcePath := resource.Get("path").(string)
	body := resource.Get("body").(string)

	path := generatePath(resourcePath, body)

	version, ok := resource.Get("version").(int)
	if !ok {
		log.Debug("cannot cast version value in int for %s", path)
	}
	log.Debug("event %s", eventType)

	if eventType == "create" || eventType == "update" {
		log.Debug("set %s on sync", path)
		content, err := json.Marshal(map[string]interface{}{
			"body":    body,
			"version": version,
		})
		if err != nil {
			log.Error(fmt.Sprintf("When marshalling sync object: %s", err))
			return err
		}
		err = server.sync.Update(path, string(content))
		if err != nil {
			log.Error(fmt.Sprintf("%s on sync", err))
			return err
		}
	} else if eventType == "delete" {
		log.Debug("delete %s", resourcePath)
		deletePath := resourcePath
		resourceSchema := schema.GetSchemaByURLPath(resourcePath)
		if _, ok := resourceSchema.SyncKeyTemplate(); ok {
			var data map[string]interface{}
			err := json.Unmarshal(([]byte)(body), &data)
			deletePath, err = resourceSchema.GenerateCustomPath(data)
			if err != nil {
				log.Error(fmt.Sprintf("Delete from sync failed %s - generating of custom path failed", err))
				return err
			}
		}
		log.Debug("deleting %s", statePrefix+deletePath)
		err = server.sync.Delete(statePrefix + deletePath)
		if err != nil {
			log.Error(fmt.Sprintf("Delete from sync failed %s", err))
		}
		log.Debug("deleting %s", monitoringPrefix+deletePath)
		err = server.sync.Delete(monitoringPrefix + deletePath)
		if err != nil {
			log.Error(fmt.Sprintf("Delete from sync failed %s", err))
		}
		log.Debug("deleting %s", resourcePath)
		err = server.sync.Delete(path)
		if err != nil {
			log.Error(fmt.Sprintf("Delete from sync failed %s", err))
			return err
		}
	}
	log.Debug("delete event %d", resource.Get("id"))
	id := resource.Get("id")
	err = tx.Delete(eventSchema, id)
	if err != nil {
		log.Error(fmt.Sprintf("delete failed: %s", err))
		return err
	}

	err = tx.Commit()
	if err != nil {
		log.Error(fmt.Sprintf("commit failed: %s", err))
		return err
	}
	return nil
}

func generatePath(resourcePath string, body string) string {
	var curSchema = schema.GetSchemaByURLPath(resourcePath)
	path := resourcePath
	if _, ok := curSchema.SyncKeyTemplate(); ok {
		var data map[string]interface{}
		err := json.Unmarshal(([]byte)(body), &data)
		if err != nil {
			log.Error(fmt.Sprintf("Error %v during unmarshaling data %v", err, data))
		} else {
			path, err = curSchema.GenerateCustomPath(data)
			if err != nil {
				path = resourcePath
				log.Error(fmt.Sprintf("%v", err))
			}
		}
	}
	path = configPrefix + path
	log.Info("Generated path: %s", path)
	return path
}

//Start sync Process
func startSyncProcess(server *Server) {
	pollingTicker := time.Tick(eventPollingTime)
	committed := transactionCommitInformer()
	go func() {
		defer util.LogFatalPanic(log)
		recentlySynced := false
		for server.running {
			select {
			case <-pollingTicker:
				if recentlySynced {
					recentlySynced = false
					continue
				}
			case <-committed:
				recentlySynced = true
			}
			server.sync.Lock(syncPath, true)
			server.Sync()
		}
		server.sync.Unlock(syncPath)
	}()
}

//Stop Sync Process
func stopSyncProcess(server *Server) {
	server.sync.Unlock(syncPath)
}

//Sync to sync backend database table
func (server *Server) Sync() error {
	resourceList, err := server.listEvents()
	if err != nil {
		return err
	}
	for _, resource := range resourceList {
		err = server.syncEvent(resource)
		if err != nil {
			return err
		}
	}
	return nil
}

//StateUpdate updates the state in the db based on the sync event
func StateUpdate(response *gohan_sync.Event, server *Server) error {
	dataStore := server.db
	schemaPath := "/" + strings.TrimPrefix(response.Key, statePrefix)
	var curSchema = schema.GetSchemaByPath(schemaPath)
	if curSchema == nil || !curSchema.StateVersioning() {
		log.Debug("State update on unexpected path '%s'", schemaPath)
		return nil
	}
	resourceID := curSchema.GetResourceIDFromPath(schemaPath)
	log.Info("Started StateUpdate for %s %s %v", response.Action, response.Key, response.Data)

	tx, err := dataStore.Begin()
	if err != nil {
		return err
	}
	defer tx.Close()
	err = tx.SetIsolationLevel(transaction.GetIsolationLevel(curSchema, StateUpdateEventName))
	if err != nil {
		return err
	}
	curResource, err := tx.Fetch(curSchema, transaction.IDFilter(resourceID))
	if err != nil {
		return err
	}
	resourceState, err := tx.StateFetch(curSchema, transaction.IDFilter(resourceID))
	if err != nil {
		return err
	}
	if resourceState.StateVersion == resourceState.ConfigVersion {
		return nil
	}
	stateVersion, ok := response.Data["version"].(float64)
	if !ok {
		return fmt.Errorf("No version in state information")
	}
	oldStateVersion := resourceState.StateVersion
	resourceState.StateVersion = int64(stateVersion)
	if resourceState.StateVersion < oldStateVersion {
		return nil
	}
	if newError, ok := response.Data["error"].(string); ok {
		resourceState.Error = newError
	}
	if newState, ok := response.Data["state"].(string); ok {
		resourceState.State = newState
	}

	environmentManager := extension.GetManager()
	environment, haveEnvironment := environmentManager.GetEnvironment(curSchema.ID)
	context := map[string]interface{}{}

	if haveEnvironment {
		serviceAuthorization, err := server.keystoneIdentity.GetServiceAuthorization()
		if err != nil {
			return err
		}

		context["catalog"] = serviceAuthorization.Catalog()
		context["auth_token"] = serviceAuthorization.AuthToken()
		context["resource"] = curResource.Data()
		context["schema"] = curSchema
		context["state"] = response.Data
		context["config_version"] = resourceState.ConfigVersion
		context["transaction"] = tx

		if err := extension.HandleEvent(context, environment, "pre_state_update_in_transaction"); err != nil {
			return err
		}
	}

	err = tx.StateUpdate(curResource, &resourceState)
	if err != nil {
		return err
	}

	if haveEnvironment {
		if err := extension.HandleEvent(context, environment, "post_state_update_in_transaction"); err != nil {
			return err
		}
	}

	return tx.Commit()
}

//MonitoringUpdate updates the state in the db based on the sync event
func MonitoringUpdate(response *gohan_sync.Event, server *Server) error {
	dataStore := server.db
	schemaPath := "/" + strings.TrimPrefix(response.Key, monitoringPrefix)
	var curSchema = schema.GetSchemaByPath(schemaPath)
	if curSchema == nil || !curSchema.StateVersioning() {
		log.Debug("Monitoring update on unexpected path '%s'", schemaPath)
		return nil
	}
	resourceID := curSchema.GetResourceIDFromPath(schemaPath)
	log.Info("Started MonitoringUpdate for %s %s %v", response.Action, response.Key, response.Data)

	tx, err := dataStore.Begin()
	if err != nil {
		return err
	}
	defer tx.Close()
	err = tx.SetIsolationLevel(transaction.GetIsolationLevel(curSchema, MonitoringUpdateEventName))
	if err != nil {
		return err
	}
	curResource, err := tx.Fetch(curSchema, transaction.IDFilter(resourceID))
	if err != nil {
		return err
	}
	resourceState, err := tx.StateFetch(curSchema, transaction.IDFilter(resourceID))
	if err != nil {
		return err
	}
	if resourceState.ConfigVersion != resourceState.StateVersion {
		log.Debug("Skipping MonitoringUpdate, because config version (%s) != state version (%s)",
			resourceState.ConfigVersion, resourceState.StateVersion)
		return nil
	}
	var ok bool
	monitoringVersion, ok := response.Data["version"].(float64)
	if !ok {
		return fmt.Errorf("No version in monitoring information")
	}
	if resourceState.ConfigVersion != int64(monitoringVersion) {
		return nil
	}
	resourceState.Monitoring, ok = response.Data["monitoring"].(string)
	if !ok {
		return fmt.Errorf("No monitoring in monitoring information")
	}

	environmentManager := extension.GetManager()
	environment, haveEnvironment := environmentManager.GetEnvironment(curSchema.ID)
	context := map[string]interface{}{}
	context["resource"] = curResource.Data()
	context["schema"] = curSchema
	context["monitoring"] = resourceState.Monitoring
	context["transaction"] = tx

	if haveEnvironment {
		if err := extension.HandleEvent(context, environment, "pre_monitoring_update_in_transaction"); err != nil {
			return err
		}
	}

	err = tx.StateUpdate(curResource, &resourceState)
	if err != nil {
		return err
	}

	if haveEnvironment {
		if err := extension.HandleEvent(context, environment, "post_monitoring_update_in_transaction"); err != nil {
			return err
		}
	}

	return tx.Commit()
}

//TODO(nati) integrate with watch process
func startStateUpdatingProcess(server *Server) {

	stateResponseChan := make(chan *gohan_sync.Event)
	stateStopChan := make(chan bool)

	if _, err := server.sync.Fetch(statePrefix); err != nil {
		server.sync.Update(statePrefix, "")
	}

	if _, err := server.sync.Fetch(monitoringPrefix); err != nil {
		server.sync.Update(monitoringPrefix, "")
	}

	go func() {
		defer util.LogFatalPanic(log)
		for server.running {
			lockKey := lockPath + "state"
			err := server.sync.Lock(lockKey, true)
			if err != nil {
				log.Warning("Can't start state watch process due to lock", err)
				time.Sleep(5 * time.Second)
				continue
			}
			defer func() {
				server.sync.Unlock(lockKey)
			}()

			err = server.sync.Watch(statePrefix, stateResponseChan, stateStopChan)
			if err != nil {
				log.Error(fmt.Sprintf("sync watch error: %s", err))
			}
		}
	}()
	go func() {
		defer util.LogFatalPanic(log)
		for server.running {
			response := <-stateResponseChan
			go func() {
				err := StateUpdate(response, server)
				if err != nil {
					log.Warning(fmt.Sprintf("error during state update: %s", err))
				}
				log.Info("Completed StateUpdate")
			}()
		}
		stateStopChan <- true
	}()
	monitoringResponseChan := make(chan *gohan_sync.Event)
	monitoringStopChan := make(chan bool)
	go func() {
		defer util.LogFatalPanic(log)
		for server.running {
			lockKey := lockPath + "monitoring"
			err := server.sync.Lock(lockKey, true)
			if err != nil {
				log.Warning("Can't start state watch process due to lock", err)
				time.Sleep(5 * time.Second)
				continue
			}
			defer func() {
				server.sync.Unlock(lockKey)
			}()
			err = server.sync.Watch(monitoringPrefix, monitoringResponseChan, monitoringStopChan)
			if err != nil {
				log.Error(fmt.Sprintf("sync watch error: %s", err))
			}
		}
	}()
	go func() {
		defer util.LogFatalPanic(log)
		for server.running {
			response := <-monitoringResponseChan
			go func() {
				err := MonitoringUpdate(response, server)
				if err != nil {
					log.Warning(fmt.Sprintf("error during monitoring update: %s", err))
				}
				log.Info("Completed MonitoringUpdate")
			}()
		}
		monitoringStopChan <- true
	}()
}

func stopStateUpdatingProcess(server *Server) {
}

//Run extension on sync
func runExtensionOnSync(server *Server, response *gohan_sync.Event, env extension.Environment) {
	context := map[string]interface{}{
		"action": response.Action,
		"data":   response.Data,
		"key":    response.Key,
	}
	if err := env.HandleEvent("notification", context); err != nil {
		log.Warning(fmt.Sprintf("extension error: %s", err))
		return
	}
	return
}

//Sync Watch Process
func startSyncWatchProcess(server *Server) {
	config := util.GetConfig()
	watch := config.GetStringList("watch/keys", nil)
	events := config.GetStringList("watch/events", nil)
	if watch == nil {
		return
	}
	extensions := map[string]extension.Environment{}
	for _, event := range events {
		path := "sync://" + event
		env, err := server.NewEnvironmentForPath("sync."+event, path)
		if err != nil {
			log.Fatal(err.Error())
		}
		extensions[event] = env
	}
	responseChan := make(chan *gohan_sync.Event)
	stopChan := make(chan bool)
	for _, path := range watch {
		go func(path string) {
			defer util.LogFatalPanic(log)
			for server.running {
				lockKey := lockPath + "watch"
				err := server.sync.Lock(lockKey, true)
				if err != nil {
					log.Warning("Can't start watch process due to lock", err)
					time.Sleep(5 * time.Second)
					continue
				}
				defer func() {
					server.sync.Unlock(lockKey)
				}()
				err = server.sync.Watch(path, responseChan, stopChan)
				if err != nil {
					log.Error(fmt.Sprintf("sync watch error: %s", err))
				}
			}
		}(path)
	}
	//main response lisnter process
	go func() {
		defer util.LogFatalPanic(log)
		for server.running {
			response := <-responseChan
			server.queue.Add(job.NewJob(
				func() {
					defer util.LogPanic(log)
					for _, event := range events {
						//match extensions
						if strings.HasPrefix(response.Key, "/"+event) {
							env := extensions[event]
							runExtensionOnSync(server, response, env.Clone())
							return
						}
					}
				}))
		}
	}()

}

//Stop Watch Process
func stopSyncWatchProcess(server *Server) {
}
