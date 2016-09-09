// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package processor implements MDS plugin processor
// processor_core contains functions that fetch messages and schedule them to be executed
package processor

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/fileutil"
	"github.com/aws/amazon-ssm-agent/agent/framework/engine"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	messageContracts "github.com/aws/amazon-ssm-agent/agent/message/contracts"
	"github.com/aws/amazon-ssm-agent/agent/message/parser"
	"github.com/aws/amazon-ssm-agent/agent/message/service"
	"github.com/aws/amazon-ssm-agent/agent/platform"
	"github.com/aws/amazon-ssm-agent/agent/sdkutil"
	commandStateHelper "github.com/aws/amazon-ssm-agent/agent/statemanager"
	"github.com/aws/amazon-ssm-agent/agent/task"
	"github.com/aws/aws-sdk-go/service/ssmmds"
)

var singletonMapOfUnsupportedSSMDocs map[string]bool
var once sync.Once

var loadDocStateFromSendCommand = parseSendCommandMessage
var loadDocStateFromCancelCommand = parseCancelCommandMessage

// processOlderMessages processes older messages that have been persisted in various locations (like from pending & current folders)
func (p *Processor) processOlderMessages() {
	log := p.context.Log()

	instanceID, err := platform.InstanceID()
	if err != nil {
		log.Errorf("error fetching instance id, %v", err)
		return
	}

	//process older messages from PENDING folder
	unprocessedMsgsLocation := filepath.Join(appconfig.DefaultDataStorePath,
		instanceID,
		appconfig.DefaultCommandRootDirName,
		appconfig.DefaultLocationOfState,
		appconfig.DefaultLocationOfPending)

	if isDirectoryEmpty, _ := fileutil.IsDirEmpty(unprocessedMsgsLocation); isDirectoryEmpty {

		log.Debugf("No messages to process from %v", unprocessedMsgsLocation)

	} else {

		//get all pending messages
		files, err := ioutil.ReadDir(unprocessedMsgsLocation)

		//TODO:  revisit this when bookkeeping is made invasive
		if err != nil {
			log.Errorf("skipping reading pending messages from %v. unexpected error encountered - %v", unprocessedMsgsLocation, err)
			return
		}

		//iterate through all pending messages
		for _, f := range files {
			log.Debugf("Processing an older message with messageID - %v", f.Name())

			//construct the absolute path - safely assuming that interim state for older messages are already present in Pending folder
			file := filepath.Join(appconfig.DefaultDataStorePath,
				instanceID,
				appconfig.DefaultCommandRootDirName,
				appconfig.DefaultLocationOfState,
				appconfig.DefaultLocationOfPending,
				f.Name())

			var docState messageContracts.DocumentState

			//parse the message
			if err := jsonutil.UnmarshalFile(file, &docState); err != nil {
				log.Errorf("skipping processsing of pending messages. encountered error %v while reading pending message from file - %v", err, f)
				break
			}

			if docState.IsAssociation() {
				break
			}

			p.submitDocForExecution(&docState)
		}
	}

	//process older messages from CURRENT folder
	p.processMessagesFromCurrent(instanceID)

	return
}

// processOlderMessagesFromCurrent processes older messages that were persisted in CURRENT folder
func (p *Processor) processMessagesFromCurrent(instanceID string) {
	log := p.context.Log()
	config := p.context.AppConfig()

	unprocessedMsgsLocation := filepath.Join(appconfig.DefaultDataStorePath,
		instanceID,
		appconfig.DefaultCommandRootDirName,
		appconfig.DefaultLocationOfState,
		appconfig.DefaultLocationOfCurrent)

	if isDirectoryEmpty, _ := fileutil.IsDirEmpty(unprocessedMsgsLocation); isDirectoryEmpty {

		log.Debugf("no older messages to process from %v", unprocessedMsgsLocation)

	} else {

		//get all older messages from previous run of agent
		files, err := ioutil.ReadDir(unprocessedMsgsLocation)
		//TODO:  revisit this when bookkeeping is made invasive
		if err != nil {
			log.Errorf("skipping reading inprogress messages from %v. unexpected error encountered - %v", unprocessedMsgsLocation, err)
			return
		}

		//iterate through all old executing messages
		for _, f := range files {
			log.Debugf("processing previously unexecuted message - %v", f.Name())

			//construct the absolute path - safely assuming that interim state for older messages are already present in Current folder
			file := filepath.Join(appconfig.DefaultDataStorePath,
				instanceID,
				appconfig.DefaultCommandRootDirName,
				appconfig.DefaultLocationOfState,
				appconfig.DefaultLocationOfCurrent,
				f.Name())

			var docState messageContracts.DocumentState

			//parse the message
			if err := jsonutil.UnmarshalFile(file, &docState); err != nil {
				log.Errorf("skipping processsing of previously unexecuted messages. encountered error %v while reading unprocessed message from file - %v", err, f)
				break
			}

			if docState.IsAssociation() {
				break
			}

			if docState.DocumentInformation.RunCount >= config.Mds.CommandRetryLimit {
				//TODO:  Move command to corrupt/failed
				// do not process as the command has failed too many times
				break
			}

			//TODO: fix resume cancel command
			pluginOutputs := make(map[string]*contracts.PluginResult)

			// increment the command run count
			docState.DocumentInformation.RunCount++
			// Update reboot status
			for v := range docState.PluginsInformation {
				plugin := docState.PluginsInformation[v]
				if plugin.HasExecuted && plugin.Result.Status == contracts.ResultStatusSuccessAndReboot {
					log.Debugf("plugin %v has completed a reboot. Setting status to Success.", v)
					plugin.Result.Status = contracts.ResultStatusSuccess
					docState.PluginsInformation[v] = plugin
					pluginOutputs[v] = &plugin.Result
					p.sendResponse(docState.DocumentInformation.MessageID, v, pluginOutputs)
				}
			}

			// func PersistData(log log.T, commandID, instanceID, locationFolder string, object interface{}) {
			commandStateHelper.PersistData(log, docState.DocumentInformation.CommandID, instanceID, appconfig.DefaultLocationOfCurrent, docState)

			//Submit the work to Job Pool so that we don't block for processing of new messages
			err := p.sendCommandPool.Submit(log, docState.DocumentInformation.MessageID, func(cancelFlag task.CancelFlag) {
				p.runCmdsUsingCmdState(p.context.With("[messageID="+docState.DocumentInformation.MessageID+"]"),
					p.service,
					p.pluginRunner,
					cancelFlag,
					p.buildReply,
					p.sendResponse,
					docState)
			})
			if err != nil {
				log.Error("SendCommand failed for previously unexecuted commands", err)
				break
			}
		}
	}
}

// runCmdsUsingCmdState takes commandState as an input and executes only those plugins which haven't yet executed. This is functionally
// very similar to processSendCommandMessage because everything to do with cmd execution is part of that function right now.
func (p *Processor) runCmdsUsingCmdState(context context.T,
	mdsService service.Service,
	runPlugins PluginRunner,
	cancelFlag task.CancelFlag,
	buildReply replyBuilder,
	sendResponse engine.SendResponse,
	command messageContracts.DocumentState) {

	log := context.Log()
	var pluginConfigurations map[string]*contracts.Configuration
	pendingPlugins := false
	pluginConfigurations = make(map[string]*contracts.Configuration)

	//iterate through all plugins to find all plugins that haven't executed yet.
	for k, v := range command.PluginsInformation {
		if v.HasExecuted {
			log.Debugf("skipping execution of Plugin - %v of command - %v since it has already executed.", k, command.DocumentInformation.CommandID)
		} else {
			log.Debugf("Plugin - %v of command - %v will be executed", k, command.DocumentInformation.CommandID)
			pluginConfigurations[k] = &v.Configuration
			pendingPlugins = true
		}
	}

	//execute plugins that haven't been executed yet
	//individual plugins after execution will update interim cmd state file accordingly
	if pendingPlugins {

		log.Debugf("executing following plugins of command - %v", command.DocumentInformation.CommandID)
		for k := range pluginConfigurations {
			log.Debugf("Plugin: %v", k)
		}

		//Since only some plugins of a cmd gets executed here - there is no need to get output from engine & construct the sendReply output.
		//Instead after all plugins of a command get executed, use persisted data to construct sendReply payload
		runPlugins(context, command.DocumentInformation.MessageID, pluginConfigurations, sendResponse, cancelFlag)
	}

	//read from persisted file
	newCmdState := commandStateHelper.GetCommandInterimState(log,
		command.DocumentInformation.CommandID,
		command.DocumentInformation.Destination,
		appconfig.DefaultLocationOfCurrent)

	//construct sendReply payload
	outputs := make(map[string]*contracts.PluginResult)

	for k, v := range newCmdState.PluginsInformation {
		outputs[k] = &v.Result
	}

	pluginOutputContent, _ := jsonutil.Marshal(outputs)
	log.Debugf("plugin outputs %v", jsonutil.Indent(pluginOutputContent))

	payloadDoc := buildReply("", outputs)

	//update interim cmd state file with document level information
	var documentInfo messageContracts.DocumentInfo

	// set document level information which wasn't set previously
	documentInfo.AdditionalInfo = payloadDoc.AdditionalInfo
	documentInfo.DocumentStatus = payloadDoc.DocumentStatus
	documentInfo.DocumentTraceOutput = payloadDoc.DocumentTraceOutput
	documentInfo.RuntimeStatus = payloadDoc.RuntimeStatus

	//persist final documentInfo.
	commandStateHelper.PersistDocumentInfo(log,
		documentInfo,
		command.DocumentInformation.CommandID,
		command.DocumentInformation.Destination,
		appconfig.DefaultLocationOfCurrent)

	// Skip sending response when the document requires a reboot
	if documentInfo.DocumentStatus == contracts.ResultStatusSuccessAndReboot {
		log.Debug("skipping sending response of %v since the document requires a reboot", newCmdState.DocumentInformation.MessageID)
		return
	}

	//send document level reply
	log.Debug("sending reply on message completion ", outputs)
	sendResponse(command.DocumentInformation.MessageID, "", outputs)

	//persist : commands execution in completed folder (terminal state folder)
	log.Debugf("execution of %v is over. Moving interimState file from Current to Completed folder", newCmdState.DocumentInformation.MessageID)

	commandStateHelper.MoveCommandState(log,
		newCmdState.DocumentInformation.CommandID,
		newCmdState.DocumentInformation.Destination,
		appconfig.DefaultLocationOfCurrent,
		appconfig.DefaultLocationOfCompleted)

	log.Debugf("deleting message")
	isUpdate := false
	for pluginName := range pluginConfigurations {
		if pluginName == appconfig.PluginNameAwsAgentUpdate {
			isUpdate = true
		}
	}
	if !isUpdate {
		err := mdsService.DeleteMessage(log, newCmdState.DocumentInformation.MessageID)
		if err != nil {
			sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
		}
	} else {
		log.Debug("messageDeletion skipped as it will be handled by external process")
	}
}

func (p *Processor) processMessage(msg *ssmmds.Message) {
	var (
		docState *messageContracts.DocumentState
		err      error
	)

	// create separate logger that includes messageID with every log message
	context := p.context.With("[messageID=" + *msg.MessageId + "]")
	log := context.Log()
	log.Debug("Processing message")

	if err = validate(msg); err != nil {
		log.Error("message not valid, ignoring: ", err)
		return
	}

	if strings.HasPrefix(*msg.Topic, string(SendCommandTopicPrefix)) {
		docState, err = loadDocStateFromSendCommand(context, msg, p.orchestrationRootDir)
	} else if strings.HasPrefix(*msg.Topic, string(CancelCommandTopicPrefix)) {
		docState, err = loadDocStateFromCancelCommand(context, msg, p.orchestrationRootDir)
	} else {
		err = fmt.Errorf("unexpected topic name %v", *msg.Topic)
	}

	if err != nil {
		log.Error("format of received message is invalid ", err)
		if err = p.service.FailMessage(log, *msg.MessageId, service.InternalHandlerException); err != nil {
			sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
		}
		return
	}

	//persisting received msg in file-system [pending folder]
	p.persistData(docState, appconfig.DefaultLocationOfPending)
	if err = p.service.AcknowledgeMessage(log, *msg.MessageId); err != nil {
		sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
		return
	}

	log.Debugf("Ack done. Received message - messageId - %v, MessageString - %v", *msg.MessageId, msg.GoString())
	log.Debugf("Processing to send a reply to update the document status to InProgress")

	p.sendDocLevelResponse(*msg.MessageId, contracts.ResultStatusInProgress, "")

	log.Debugf("SendReply done. Received message - messageId - %v, MessageString - %v", *msg.MessageId, msg.GoString())

	p.submitDocForExecution(docState)
}

// submitDocForExecution moves doc to current folder and submit it for execution
func (p *Processor) submitDocForExecution(docState *messageContracts.DocumentState) {
	log := p.context.Log()

	commandStateHelper.MoveCommandState(log,
		docState.DocumentInformation.CommandID,
		docState.DocumentInformation.Destination,
		appconfig.DefaultLocationOfPending,
		appconfig.DefaultLocationOfCurrent)

	switch docState.DocumentType {
	case messageContracts.SendCommand:
		err := p.sendCommandPool.Submit(log, docState.DocumentInformation.MessageID, func(cancelFlag task.CancelFlag) {
			p.processSendCommandMessage(
				p.context,
				p.service,
				p.orchestrationRootDir,
				p.pluginRunner,
				cancelFlag,
				p.buildReply,
				p.sendResponse,
				docState)
		})
		if err != nil {
			log.Error("SendCommand failed", err)
			return
		}

	case messageContracts.CancelCommand:
		err := p.cancelCommandPool.Submit(log, docState.DocumentInformation.MessageID, func(cancelFlag task.CancelFlag) {
			p.processCancelCommandMessage(p.context, p.service, p.sendCommandPool, docState)
		})
		if err != nil {
			log.Error("CancelCommand failed", err)
			return
		}

	default:
		log.Error("unexpected document type ", docState.DocumentType)
	}
}

// loadPluginConfigurations returns plugin configurations that hasn't been executed
func loadPluginConfigurations(plugins map[string]messageContracts.PluginState) map[string]*contracts.Configuration {
	configs := make(map[string]*contracts.Configuration)

	for pluginName, pluginConfig := range plugins {
		if pluginConfig.HasExecuted {
			break
		}
		configs[pluginName] = &pluginConfig.Configuration
	}

	return configs
}

// processSendCommandMessage processes a single send command message received from MDS.
func (p *Processor) processSendCommandMessage(context context.T,
	mdsService service.Service,
	messagesOrchestrationRootDir string,
	runPlugins PluginRunner,
	cancelFlag task.CancelFlag,
	buildReply replyBuilder,
	sendResponse engine.SendResponse,
	docState *messageContracts.DocumentState) {

	log := context.Log()

	pluginConfigurations := loadPluginConfigurations(docState.PluginsInformation)
	log.Debug("Running plugins...")
	outputs := runPlugins(context, docState.DocumentInformation.MessageID, pluginConfigurations, sendResponse, cancelFlag)
	pluginOutputContent, _ := jsonutil.Marshal(outputs)
	log.Debugf("Plugin outputs %v", jsonutil.Indent(pluginOutputContent))

	payloadDoc := buildReply("", outputs)

	//update documentInfo in interim cmd state file
	documentInfo := commandStateHelper.GetDocumentInfo(log, docState.DocumentInformation.CommandID, docState.DocumentInformation.Destination, appconfig.DefaultLocationOfCurrent)

	// set document level information which wasn't set previously
	documentInfo.AdditionalInfo = payloadDoc.AdditionalInfo
	documentInfo.DocumentStatus = payloadDoc.DocumentStatus
	documentInfo.DocumentTraceOutput = payloadDoc.DocumentTraceOutput
	documentInfo.RuntimeStatus = payloadDoc.RuntimeStatus

	//persist final documentInfo.
	commandStateHelper.PersistDocumentInfo(log,
		documentInfo,
		docState.DocumentInformation.CommandID,
		docState.DocumentInformation.Destination,
		appconfig.DefaultLocationOfCurrent)

	// Skip sending response when the document requires a reboot
	if documentInfo.DocumentStatus == contracts.ResultStatusSuccessAndReboot {
		log.Debug("skipping sending response of %v since the document requires a reboot", docState.DocumentInformation.MessageID)
		return
	}

	log.Debug("Sending reply on message completion ", outputs)
	sendResponse(docState.DocumentInformation.MessageID, "", outputs)

	//persist : commands execution in completed folder (terminal state folder)
	log.Debugf("execution of %v is over. Moving interimState file from Current to Completed folder", docState.DocumentInformation.MessageID)

	commandStateHelper.MoveCommandState(log,
		docState.DocumentInformation.CommandID,
		docState.DocumentInformation.Destination,
		appconfig.DefaultLocationOfCurrent,
		appconfig.DefaultLocationOfCompleted)

	log.Debugf("Deleting message")
	isUpdate := false
	for pluginName := range pluginConfigurations {
		if pluginName == appconfig.PluginNameAwsAgentUpdate {
			isUpdate = true
		}
	}
	if !isUpdate {
		if err := mdsService.DeleteMessage(log, docState.DocumentInformation.MessageID); err != nil {
			sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
		}
	} else {
		log.Debug("MessageDeletion skipped as it will be handled by external process")
	}
}

func parseSendCommandMessage(context context.T, msg *ssmmds.Message, messagesOrchestrationRootDir string) (*messageContracts.DocumentState, error) {
	log := context.Log()
	commandID := getCommandID(*msg.MessageId)

	log.Debug("Processing send command message ", *msg.MessageId)
	log.Trace("Processing send command message ", jsonutil.Indent(*msg.Payload))

	parsedMessage, err := parser.ParseMessageWithParams(log, *msg.Payload)
	if err != nil {
		return nil, err
	}

	parsedMessageContent, _ := jsonutil.Marshal(parsedMessage)
	log.Debug("ParsedMessage is ", jsonutil.Indent(parsedMessageContent))

	// adapt plugin configuration format from MDS to plugin expected format
	s3KeyPrefix := path.Join(parsedMessage.OutputS3KeyPrefix, parsedMessage.CommandID, *msg.Destination)

	messageOrchestrationDirectory := filepath.Join(messagesOrchestrationRootDir, commandID)

	// Check if it is a managed instance and its executing managed instance incompatible AWS SSM public document.
	// A few public AWS SSM documents contain code which is not compatible when run on managed instances.
	// isManagedInstanceIncompatibleAWSSSMDocument makes sure to find such documents at runtime and replace the incompatible code.
	isMI, err := isManagedInstance()
	if err != nil {
		log.Errorf("Error determining managed instance. error: %v", err)
	}

	if isMI && isManagedInstanceIncompatibleAWSSSMDocument(parsedMessage.DocumentName) {
		log.Debugf("Running Incompatible AWS SSM Document %v on managed instance", parsedMessage.DocumentName)
		if parsedMessage, err = removeDependencyOnInstanceMetadataForManagedInstance(context, parsedMessage); err != nil {
			return nil, err
		}
	}

	pluginConfigurations := getPluginConfigurations(
		parsedMessage.DocumentContent.RuntimeConfig,
		messageOrchestrationDirectory,
		parsedMessage.OutputS3BucketName,
		s3KeyPrefix,
		*msg.MessageId)

	//persist : all information in current folder
	log.Info("Persisting message in current execution folder")

	//Data format persisted in Current Folder is defined by the struct - CommandState
	docState := initializeSendCommandState(pluginConfigurations, *msg, parsedMessage)
	return &docState, nil
}

// processCancelCommandMessage processes a single send command message received from MDS.
func (p *Processor) processCancelCommandMessage(context context.T,
	mdsService service.Service,
	sendCommandPool task.Pool,
	docState *messageContracts.DocumentState) {

	log := context.Log()

	log.Debugf("Canceling job with id %v...", docState.CancelInformation.CancelMessageID)

	if found := sendCommandPool.Cancel(docState.CancelInformation.CancelMessageID); !found {
		log.Debugf("Job with id %v not found (possibly completed)", docState.CancelInformation.CancelMessageID)
		docState.CancelInformation.DebugInfo = fmt.Sprintf("Command %v couldn't be cancelled", docState.CancelInformation.CancelCommandID)
		docState.DocumentInformation.DocumentStatus = contracts.ResultStatusFailed
	} else {
		docState.CancelInformation.DebugInfo = fmt.Sprintf("Command %v cancelled", docState.CancelInformation.CancelCommandID)
		docState.DocumentInformation.DocumentStatus = contracts.ResultStatusSuccess
	}

	//persist the final status of cancel-message in current folder
	commandStateHelper.PersistData(log,
		docState.DocumentInformation.CommandID,
		docState.DocumentInformation.Destination,
		appconfig.DefaultLocationOfCurrent, docState)

	//persist : commands execution in completed folder (terminal state folder)
	log.Debugf("Execution of %v is over. Moving interimState file from Current to Completed folder", docState.DocumentInformation.MessageID)

	commandStateHelper.MoveCommandState(log,
		docState.DocumentInformation.CommandID,
		docState.DocumentInformation.Destination,
		appconfig.DefaultLocationOfCurrent,
		appconfig.DefaultLocationOfCompleted)

	log.Debugf("Deleting message")
	if err := mdsService.DeleteMessage(log, docState.DocumentInformation.MessageID); err != nil {
		sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
	}
}

func parseCancelCommandMessage(context context.T, msg *ssmmds.Message, messagesOrchestrationRootDir string) (*messageContracts.DocumentState, error) {
	log := context.Log()

	log.Debug("Processing cancel command message ", jsonutil.Indent(*msg.Payload))

	var parsedMessage messageContracts.CancelPayload
	err := json.Unmarshal([]byte(*msg.Payload), &parsedMessage)
	if err != nil {
		return nil, err
	}
	log.Debugf("ParsedMessage is %v", parsedMessage)

	//persist in current folder here
	docState := initializeCancelCommandState(*msg, parsedMessage)
	return &docState, nil
}
