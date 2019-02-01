package transit

import (
	"fmt"
	"math"
	"time"

	. "github.com/moleculer-go/moleculer/common"
	. "github.com/moleculer-go/moleculer/context"
	log "github.com/sirupsen/logrus"
)

type TransportHandler func(message TransitMessage)

type Transport interface {
	Subscribe(command, nodeID string, handler TransportHandler)
	MakeBalancedSubscriptions()
	Publish(command, nodeID string, message TransitMessage)
	Connect() chan bool
	Disconnect() chan bool
	Request(message TransitMessage) chan interface{}
}

type TransitImpl struct {
	self                   *TransitImpl
	logger                 *log.Entry
	transport              *Transport
	broker                 *BrokerInfo
	isConnected            bool
	pendingRequests        *map[string]pendingRequest
	registryMessageHandler RegistryMessageHandlerFunction
}

func (transit *TransitImpl) onBrokerStarted(values ...interface{}) {
	if transit.isConnected {
		transit.broadcastNodeInfo("")
	}
}

func CreateTransit(broker *BrokerInfo) *Transit {
	pendingRequests := make(map[string]pendingRequest)
	transitImpl := TransitImpl{
		broker:                 broker,
		isConnected:            false,
		registryMessageHandler: broker.RegistryMessageHandler,
		pendingRequests:        &pendingRequests,
		logger:                 broker.GetLogger("Transit", ""),
	}
	transitImpl.self = &transitImpl

	broker.GetLocalBus().On("$node.disconnected", transitImpl.onNodeDisconnected)

	broker.GetLocalBus().On("$broker.started", transitImpl.onBrokerStarted)

	var transit Transit = transitImpl
	return &transit
}

func (transit *TransitImpl) onNodeDisconnected(values ...interface{}) {
	var nodeID string = values[0].(string)
	transit.logger.Debug("onNodeDisconnected() nodeID: ", nodeID)
	pending := (*transit.pendingRequests)[nodeID]
	(*pending.resultChan) <- fmt.Errorf("Node %s disconnected. Request being canceled.", nodeID)
	delete((*transit.pendingRequests), nodeID)
}

// CreateTransport : based on config it will load the transporter
// for now is hard coded for NATS Streaming localhost
func CreateTransport(broker *BrokerInfo) *Transport {
	//TODO: move this to config and params
	prefix := "MOL"
	url := "stan://localhost:4222"
	clusterID := "test-cluster"

	localNodeID := (*broker.GetLocalNode()).GetID()
	serializer := broker.GetSerializer()
	logger := broker.GetLogger("transport", "stan")

	options := StanTransporterOptions{
		prefix,
		url,
		clusterID,
		localNodeID,
		logger,
		serializer,
		func(message *TransitMessage) bool {
			sender := (*message).Get("sender").String()
			return sender != localNodeID
		},
	}

	var transport Transport = CreateStanTransporter(options)
	return &transport
}

type pendingRequest struct {
	context    *Context
	resultChan *chan interface{}
}

func (transit *TransitImpl) checkMaxQueueSize() {
	//TODO: check transit.js line 524
}

//DiscoverNodes will check if there are neighbours and return true if any are found ;).
func (transit TransitImpl) DiscoverNodes() chan bool {
	result := make(chan bool)
	go func() {
		//TODO: implement the discover protocol usinga  request pattern so we wait for response OR timeout.
		transit.DiscoverNode("")
		result <- true
	}()
	return result
}

func (transit TransitImpl) SendHeartbeat() {
	transit.self.sendHeartbeatImpl()
}

func (transit *TransitImpl) sendHeartbeatImpl() {
	node := (*transit.broker.GetLocalNode()).ExportAsMap()
	payload := map[string]interface{}{
		"sender": node["id"],
		"cpu":    node["cpu"],
		"cpuSeq": node["cpuSeq"],
	}
	message, err := (*transit.broker.GetSerializer()).MapToMessage(&payload)
	if err == nil {
		(*transit.transport).Publish("HEARTBEAT", "", message)
	}
}

func (transit TransitImpl) DiscoverNode(nodeID string) {
	transit.self.discoverNodeImpl(nodeID)
}
func (transit TransitImpl) discoverNodeImpl(nodeID string) {
	payload := map[string]interface{}{"sender": (*transit.broker.GetLocalNode()).GetID()}
	message, err := (*transit.broker.GetSerializer()).MapToMessage(&payload)
	if err == nil {
		(*transit.transport).Publish("DISCOVER", nodeID, message)
	}
}

func (transit *TransitImpl) requestImpl(context *Context) chan interface{} {

	transit.checkMaxQueueSize()

	resultChan := make(chan interface{})

	targetNodeID := (*context).GetTargetNodeID()
	payload := (*context).AsMap()
	payload["sender"] = (*transit.broker.GetLocalNode()).GetID()

	transit.logger.Trace("requestImpl() targetNodeID: ", targetNodeID, " payload: ", payload)

	message, err := (*transit.broker.GetSerializer()).MapToMessage(&payload)
	if err != nil {
		transit.logger.Error("Request() Error serializing the payload: ", payload, " error: ", err)
		panic(fmt.Errorf("Error trying to serialize the payload. Likely issues with the action params. Error: %s", err))
	}
	(*transit.pendingRequests)[(*context).GetID()] = pendingRequest{
		context,
		&resultChan,
	}

	(*transit.transport).Publish("REQ", targetNodeID, message)
	return resultChan
}

func (transit TransitImpl) Request(context *Context) chan interface{} {
	return (*transit.self).requestImpl(context)
}

func (transit *TransitImpl) reponseHandler() TransportHandler {
	return func(message TransitMessage) {
		id := message.Get("id").String()
		sender := message.Get("sender").String()
		transit.logger.Debug("reponseHandler() - response arrived from nodeID: ", sender, " id: ", id)

		request := (*transit.pendingRequests)[id]
		delete((*transit.pendingRequests), id)
		result := message.Get("data").Value()

		transit.logger.Trace("reponseHandler() id: ", id, " result: ", result)
		go func() {
			(*request.resultChan) <- result
		}()
	}
}

func (transit *TransitImpl) sendResponse(context *Context, response interface{}) {
	targetNodeID := (*context).GetTargetNodeID()

	payload := make(map[string]interface{})
	payload["sender"] = (*transit.broker.GetLocalNode()).GetID()
	payload["id"] = (*context).GetID()
	payload["meta"] = (*context).GetMeta()
	payload["success"] = true
	payload["data"] = response

	message, err := (*transit.broker.GetSerializer()).MapToMessage(&payload)
	if err != nil {
		transit.logger.Error("sendResponse() Erro serializing the payload: ", payload, " error: ", err)
		panic(err)
	}

	transit.logger.Debug("sendResponse() targetNodeID: ", targetNodeID, " payload: ", payload)

	(*transit.transport).Publish("RES", targetNodeID, message)
}

// requestHandler : handles when a request arrives on this node.
// 1: create a context from the message, the context contains the target action
// 2: invoke the action
// 3: send a response
func (transit *TransitImpl) requestHandler() TransportHandler {
	return func(message TransitMessage) {
		values := (*transit.broker.GetSerializer()).MessageToContextMap(&message)
		context := CreateContext(transit.broker, values)
		result := <-context.InvokeAction()
		transit.sendResponse(&context, result)
	}
}

//TODO
func (transit *TransitImpl) eventHandler() TransportHandler {
	return func(message TransitMessage) {
		//context := (*transit.serializer).MessageToContext(&message)
		// result := <-context.InvokeAction()
		// transit.sendResponse(&context, result)
	}
}

func (transit *TransitImpl) broadcastNodeInfo(targetNodeID string) {
	payload := (*transit.broker.GetLocalNode()).ExportAsMap()
	payload["sender"] = payload["id"]
	message, _ := (*transit.broker.GetSerializer()).MapToMessage(&payload)
	(*transit.transport).Publish("INFO", targetNodeID, message)
}

func (transit *TransitImpl) discoverHandler() TransportHandler {
	return func(message TransitMessage) {
		sender := message.Get("sender").String()
		transit.broadcastNodeInfo(sender)
	}
}

func (transit *TransitImpl) registryDelegateHandler(command string) TransportHandler {
	return func(message TransitMessage) {
		transit.registryMessageHandler(command, &message)
	}
}

func (transit *TransitImpl) SendPing() {
	ping := make(map[string]interface{})
	sender := (*transit.broker.GetLocalNode()).GetID()
	ping["sender"] = sender
	ping["time"] = time.Now().Unix()
	pingMessage, _ := (*transit.broker.GetSerializer()).MapToMessage(&ping)
	(*transit.transport).Publish("PING", sender, pingMessage)

}

func (transit *TransitImpl) pingHandler() TransportHandler {
	return func(message TransitMessage) {
		pong := make(map[string]interface{})
		sender := message.Get("sender").String()
		pong["sender"] = sender
		pong["time"] = message.Get("time").Int()
		pong["arrived"] = time.Now().Unix()

		pongMessage, _ := (*transit.broker.GetSerializer()).MapToMessage(&pong)
		(*transit.transport).Publish("PONG", sender, pongMessage)
	}
}

func (transit *TransitImpl) pongHandler() TransportHandler {
	return func(message TransitMessage) {
		now := time.Now().Unix()
		elapsed := now - message.Get("time").Int()
		arrived := message.Get("arrived").Int()
		timeDiff := math.Round(
			float64(now) - float64(arrived) - float64(elapsed)/2)

		mapValue := make(map[string]interface{})
		mapValue["nodeID"] = message.Get("sender").String()
		mapValue["elapsedTime"] = elapsed
		mapValue["timeDiff"] = timeDiff

		transit.broker.GetLocalBus().EmitAsync("$node.pong", []interface{}{mapValue})
	}
}

func (transit *TransitImpl) subscribe() {
	nodeID := (*transit.broker.GetLocalNode()).GetID()
	(*transit.transport).Subscribe("RES", nodeID, transit.reponseHandler())
	(*transit.transport).Subscribe("REQ", nodeID, transit.requestHandler())

	(*transit.transport).Subscribe("HEARTBEAT", "", transit.registryDelegateHandler("HEARTBEAT"))
	(*transit.transport).Subscribe("DISCONNECT", "", transit.registryDelegateHandler("DISCONNECT"))
	(*transit.transport).Subscribe("INFO", "", transit.registryDelegateHandler("INFO"))
	(*transit.transport).Subscribe("INFO", nodeID, transit.registryDelegateHandler("INFO"))
	(*transit.transport).Subscribe("EVENT", nodeID, transit.eventHandler())
	(*transit.transport).Subscribe("DISCOVER", nodeID, transit.discoverHandler())
	(*transit.transport).Subscribe("DISCOVER", "", transit.discoverHandler())
	(*transit.transport).Subscribe("PING", nodeID, transit.pingHandler())
	(*transit.transport).Subscribe("PONG", nodeID, transit.pongHandler())

}

// Connect : connect the transit with the transporter, subscribe to all events and start publishing its node info
func (transit TransitImpl) Connect() chan bool {
	endChan := make(chan bool)
	if transit.isConnected {
		endChan <- true
		return endChan
	}
	transport := CreateTransport(transit.broker)
	transit.self.transport = transport
	go func() {
		transit.self.isConnected = <-(*transport).Connect()
		if transit.self.isConnected {
			transit.self.subscribe()
		}
		endChan <- transit.self.isConnected
	}()
	return endChan
}

func (transit TransitImpl) Ready() {

}