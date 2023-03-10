package handler

import (
	"github.com/charmbracelet/log"

	blockchain_process "github.com/blocklords/sds/blockchain/inproc"
	"github.com/blocklords/sds/categorizer/smartcontract"
	"github.com/blocklords/sds/db"
	static_abi "github.com/blocklords/sds/static/abi"

	"github.com/blocklords/sds/app/remote"
	"github.com/blocklords/sds/app/remote/message"
	"github.com/blocklords/sds/app/service"
	"github.com/blocklords/sds/common/data_type"
	"github.com/blocklords/sds/common/data_type/key_value"
	"github.com/blocklords/sds/common/smartcontract_key"
)

// return a categorized smartcontract parameters by network id and smartcontract address
func GetSmartcontract(request message.Request, logger log.Logger, parameters ...interface{}) message.Reply {
	db := parameters[0].(*db.Database)

	key, err := smartcontract_key.NewFromKeyValue(request.Parameters)
	if err != nil {
		return message.Fail("smartcontract_key.NewFromKeyValue: " + err.Error())
	}

	sm, err := smartcontract.Get(db, key)

	if err != nil {
		return message.Fail("smartcontract.Get: " + err.Error())
	}

	reply := message.Reply{
		Status:     "OK",
		Parameters: key_value.Empty().Set("smartcontract", sm),
	}

	return reply

}

// returns all smartcontract categorized smartcontracts
func GetSmartcontracts(_ message.Request, logger log.Logger, parameters ...interface{}) message.Reply {
	db := parameters[0].(*db.Database)
	smartcontracts, err := smartcontract.GetAll(db)
	if err != nil {
		return message.Fail("the database error " + err.Error())
	}

	reply := message.Reply{
		Status:     "OK",
		Message:    "",
		Parameters: key_value.Empty().Set("smartcontracts", data_type.ToMapList(smartcontracts)),
	}

	return reply
}

// Register a new smartcontract to categorizer.
func SetSmartcontract(request message.Request, logger log.Logger, parameters ...interface{}) message.Reply {
	db_con := parameters[0].(*db.Database)

	kv, err := request.Parameters.GetKeyValue("smartcontract")
	if err != nil {
		return message.Fail("missing 'smartcontract' parameter")
	}

	sm, err := smartcontract.New(kv)
	if err != nil {
		return message.Fail("request parameter -> smartcontract.New: " + err.Error())
	}

	if smartcontract.Exists(db_con, sm.Key) {
		return message.Fail("the smartcontract already in SDS Categorizer")
	}

	saveErr := smartcontract.Save(db_con, sm)
	if saveErr != nil {
		return message.Fail("database: " + saveErr.Error())
	}

	pusher, err := blockchain_process.CategorizerManagerSocket(sm.Key.NetworkId)
	if err != nil {
		return message.Fail("inproc: " + err.Error())
	}
	defer pusher.Close()

	categorizer_env, _ := service.Inprocess(service.CATEGORIZER)
	static_env, err := service.Inprocess(service.STATIC)
	if err != nil {
		logger.Fatal("new static service config", "message", err)
	}
	static_socket := remote.TcpRequestSocketOrPanic(static_env, categorizer_env)
	defer static_socket.Close()

	remote_abi, err := static_abi.Get(static_socket, sm.Key)
	if err != nil {
		return message.Fail("failed to set the ABI from SDS Static. This is an exception. It should not happen. error: " + err.Error())
	}

	smartcontracts := []*smartcontract.Smartcontract{sm}
	static_abis := []*static_abi.Abi{remote_abi}

	push := message.Request{
		Command: "new-smartcontracts",
		Parameters: map[string]interface{}{
			"smartcontracts": smartcontracts,
			"abis":           static_abis,
		},
	}
	request_string, _ := push.ToString()

	_, err = pusher.SendMessage(request_string)
	if err != nil {
		return message.Fail("send: " + err.Error())
	}

	reply := message.Reply{
		Status:     "OK",
		Message:    "",
		Parameters: key_value.Empty().Set("smartcontract", sm),
	}

	return reply
}
