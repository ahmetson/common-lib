// The sdk/subscriber package is used to register for the smartcontracts
package subscriber

import (
	"fmt"
	"time"

	"github.com/blocklords/gosds/categorizer"
	"github.com/blocklords/gosds/message"
	sds_remote "github.com/blocklords/gosds/remote"
	"github.com/blocklords/gosds/static"

	"github.com/blocklords/gosds/sdk/db"
	"github.com/blocklords/gosds/sdk/remote"

	zmq "github.com/pebbe/zmq4"
)

type Subscriber struct {
	Address           string // Account address granted for reading
	timer             *time.Timer
	socket            *sds_remote.Socket
	db                *db.KVM                    // it also keeps the topic filter
	smartcontractKeys []*static.SmartcontractKey // list of smartcontract keys

	BroadcastChan   chan message.Broadcast
	HeartbeatChan   chan message.Reply
	broadcastSocket *zmq.Socket
}

func NewSubscriber(gatewaySocket *sds_remote.Socket, db *db.KVM, address string) (*Subscriber, error) {
	smartcontractKeys := make([]*static.SmartcontractKey, 0)

	subscriber := Subscriber{
		Address:           address,
		socket:            gatewaySocket,
		db:                db,
		smartcontractKeys: smartcontractKeys,
	}

	err := subscriber.loadSmartcontracts()
	if err != nil {
		return nil, err
	}

	return &subscriber, nil
}

// the main function that starts the broadcasting.
// It first calls the smartcontract_filters. and cacshes them out.
// if there is an error, it will return them either in the Heartbeat channel
func (s *Subscriber) Start() error {
	s.HeartbeatChan = make(chan message.Reply)

	var err error

	// Run the Subscriber that is connected to the Broadcaster
	s.broadcastSocket, err = remote.NewSub(s.socket.RemoteBroadcastUrl(), s.Address)
	if err != nil {
		return fmt.Errorf("failed to establish a connection with SDS Gateway: " + err.Error())
	}
	// Subscribing to the events, but we will not call the sub.ReceiveMessage
	// until we will not get the snapshot of the missing data.
	// ZMQ will queue the data until we will not call sub.ReceiveMessage.
	for _, key := range s.smartcontractKeys {
		err := s.broadcastSocket.SetSubscribe(string(*key))
		if err != nil {
			return fmt.Errorf("failed to subscribe to the smartcontract: " + err.Error())
		}
	}

	// now create a broadcaster channel to send back to the developer the messages
	s.BroadcastChan = make(chan message.Broadcast)

	port, err := s.getSinkPort()
	if err != nil {
		return fmt.Errorf("failed to create a port for Snapshots")
	}
	go s.runSink(port, len(s.smartcontractKeys))
	for i := range s.smartcontractKeys {
		go s.snapshot(i, port)
	}

	return nil
}

func (s *Subscriber) getSinkPort() (uint, error) {
	port, err := s.socket.RemoteBroadcastPort()
	if err != nil {
		return 0, err
	}

	return port + 1, nil
}

func (s *Subscriber) snapshot(i int, port uint) {
	key := s.smartcontractKeys[i]
	limit := uint64(500)
	page := uint64(1)
	blockTimestampFrom := s.db.GetBlockTimestamp(key)
	blockTimestampTo := uint64(0)

	for {
		request := message.Request{
			Command: "snapshot_get",
			Param: map[string]interface{}{
				"smartcontract_key":    key,
				"block_timestamp_from": blockTimestampFrom,
				"block_timestamp_to":   blockTimestampTo,
				"page":                 page,
				"limit":                limit,
			},
		}

		replyParams, err := s.socket.RequestRemoteService(&request)
		if err != nil {
			panic(err)
		}

		rawTransactions := replyParams["transactions"].([]map[string]interface{})
		rawLogs := replyParams["logs"].([]map[string]interface{})
		timestamp := uint64(replyParams["block_timestamp"].(float64))

		// we fetch until all is not received
		if len(rawTransactions) == 0 {
			break
		}

		transactions := make([]*categorizer.Transaction, len(rawTransactions))
		logs := make([]*categorizer.Log, len(rawLogs))

		latestBlockNumber := uint64(0)
		for i, rawTx := range rawTransactions {
			transactions[i] = categorizer.ParseTransactionFromJson(rawTx)

			if uint64(transactions[i].BlockTimestamp) > latestBlockNumber {
				latestBlockNumber = uint64(transactions[i].BlockTimestamp)
			}
		}
		for i, rawLog := range rawLogs {
			logs[i] = categorizer.ParseLog(rawLog)
		}

		err = s.db.SetBlockTimestamp(key, latestBlockNumber)
		if err != nil {
			panic(err)
		}

		reply := message.Reply{Status: "OK", Message: "", Params: map[string]interface{}{
			"transactions":    transactions,
			"logs":            logs,
			"block_timestamp": timestamp,
		}}
		s.BroadcastChan <- message.NewBroadcast(string(*key), reply)

		if blockTimestampTo == 0 {
			blockTimestampTo = timestamp
		}
		page++
	}

	sock := sds_remote.TcpPushSocketOrPanic(port)
	sock.SendMessage("")
	sock.Close()
}

func (s *Subscriber) runSink(port uint, smartcontractAmount int) {
	sock := sds_remote.TcpPullSocketOrPanic(port)
	defer sock.Close()

	for {
		_, err := sock.RecvMessage(0)
		if err != nil {
			fmt.Println("failed to receive a message: ", err.Error())
			continue
		}

		smartcontractAmount--

		if smartcontractAmount == 0 {
			break
		}
	}

	go s.loop()
}

// The algorithm
// List of the smartcontracts by smartcontract filter
func (s *Subscriber) loadSmartcontracts() error {
	// preparing the subscriber so that we catch the first message if it was send
	// by publisher.

	smartcontracts, topicStrings, err := static.RemoteSmartcontracts(s.socket, s.db.TopicFilter())
	if err != nil {
		return err
	}

	// set the smartcontract keys
	for i, sm := range smartcontracts {
		key := sm.KeyString()

		// cache the smartcontract block timestamp
		// block timestamp is used to subscribe for the events
		blockTimestamp := s.db.GetBlockTimestamp(&key)
		if blockTimestamp == 0 {
			blockTimestamp = uint64(sm.PreDeployBlockTimestamp)
			err := s.db.SetBlockTimestamp(&key, blockTimestamp)
			if err != nil {
				return err
			}
		}

		// cache the topic string
		topicString := topicStrings[i]
		err := s.db.SetTopicString(&key, topicString)
		if err != nil {
			return err
		}

		// finally track the smartcontract
		s.smartcontractKeys = append(s.smartcontractKeys, &key)
	}

	return nil
}

func (s *Subscriber) heartbeat() {
	for {
		s.timer.Reset(time.Second * time.Duration(10))

		heartbeatReply := Heartbeat(s)
		if !heartbeatReply.IsOK() {
			s.HeartbeatChan <- heartbeatReply
			break
		}

		time.Sleep(time.Second * time.Duration(2))
	}
}

func (s *Subscriber) loop() {
	s.timer = time.AfterFunc(time.Second*time.Duration(10), func() {
		s.HeartbeatChan <- message.Reply{Status: "fail", Message: "Server is not responding"}
	})

	go s.heartbeat()

	//  Process messages from both sockets
	//  We prioritize traffic from the task ventilator

	for {
		msg_raw, err := s.broadcastSocket.RecvMessage(0)
		if err != nil {
			fmt.Println("error in sub receive")
			fmt.Println(err)
		}
		if len(msg_raw) == 0 {
			break
		}

		b, err := message.ParseBroadcast(msg_raw)
		if err != nil {
			s.BroadcastChan <- message.NewBroadcast(s.Address, message.Reply{Status: "fail", Message: "Error when parsing message " + err.Error()})
			break
		}

		//  Send results to sink
		s.BroadcastChan <- b

		if !b.IsOK() {
			break //  Exit
		}
	}
}
