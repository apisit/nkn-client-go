package nkn_sdk_go

import (
	"encoding/json"
	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/websocket"
	"github.com/nknorg/nkn/api/common"
	"github.com/nknorg/nkn/api/httpjson/client"
	. "github.com/nknorg/nkn/api/websocket/client"
	"github.com/nknorg/nkn/core/ledger"
	"github.com/nknorg/nkn/crypto"
	"github.com/nknorg/nkn/util/address"
	"github.com/nknorg/nkn/vault"
	"github.com/pkg/errors"
	"log"
	"net/url"
	"time"
)

var SeedRPCServerAddr = "http://testnet-seed-0001.nkn.org:30003"
var ReconnectInterval time.Duration = 1

type Client struct {
	Address   string
	urlString string
	conn      *websocket.Conn
	closed    bool
	OnConnect chan struct{}
	OnMessage chan *InboundMessage
	OnBlock   chan *ledger.Block
}

func Init() {
	crypto.SetAlg("")
}

func call(action string, params map[string]interface{}) (interface{}, error) {
	data, err := client.Call(SeedRPCServerAddr, action, 0, params)
	resp := make(map[string]interface{})
	err = json.Unmarshal(data, &resp)
	if err != nil {
		return nil, err
	}
	if resp["error"] != nil {
		return nil, errors.New(resp["error"].(map[string]interface{})["message"].(string))
	}

	return resp["result"], nil
}

func (c *Client) connect(account *vault.Account, identifier string, force bool) error {
	if force {
		pubKey, err := account.PubKey().EncodePoint(true)
		if err != nil {
			return err
		}
		c.Address = address.MakeAddressString(pubKey, identifier)
		result, err := call("getwsaddr", map[string]interface{}{"address": c.Address})
		if err != nil {
			return err
		}
		host := result.(string)
		c.urlString = (&url.URL{Scheme: "ws", Host: host}).String()
	}

	conn, _, err := websocket.DefaultDialer.Dial(c.urlString, nil)
	if err != nil {
		return err
	}
	c.conn = conn
	c.OnConnect = make(chan struct{})
	c.OnMessage = make(chan *InboundMessage)
	c.OnBlock = make(chan *ledger.Block)

	go func() {
		defer func() {
			close(c.OnConnect)
			close(c.OnMessage)
			close(c.OnBlock)

			_ = c.conn.Close()
		}()

		force := false
		err := func() error {
			req := make(map[string]interface{})
			req["Action"] = "setClient"
			req["Addr"] = c.Address
			err := conn.WriteJSON(req)
			if err != nil {
				return err
			}

			for {
				msgType, data, err := conn.ReadMessage()
				if err != nil {
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						return err
					}
					return nil
				}

				switch msgType {
				case websocket.TextMessage:
					msg := make(map[string]*json.RawMessage)
					err := json.Unmarshal(data, &msg)
					if err != nil {
						return err
					}
					var errCode common.ErrCode
					err = json.Unmarshal(*msg["Error"], &errCode)
					if err != nil {
						return err
					}
					if errCode == common.WRONG_NODE {
						force = true
						return nil
					} else if errCode != common.SUCCESS {
						return errors.New(common.ErrMessage[errCode])
					}
					var action string
					err = json.Unmarshal(*msg["Action"], &action)
					if err != nil {
						return err
					}
					switch action {
					case "setClient":
						c.OnConnect <- struct{}{}
					case "sendRawBlock":
						block := new(ledger.Block)
						err := block.UnmarshalJson(*msg["Result"])
						if err != nil {
							return err
						}
						c.OnBlock <- block
					}
				case websocket.BinaryMessage:
					msg := &InboundMessage{}
					err := proto.Unmarshal(data, msg)
					if err != nil {
						return err
					}
					c.OnMessage <- msg
				}
			}
		}()

		if err != nil {
			log.Println(err)
		}

		if !c.closed {
			defer func() {
				time.Sleep(ReconnectInterval * time.Second)

				err = c.connect(account, identifier, force)
				if err != nil {
					log.Println(err)
				}
			}()
		}
	}()

	return nil
}

func NewClient(account *vault.Account, identifier string) (*Client, error) {
	c := Client{}
	err := c.connect(account, identifier, true)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Client) Send(dests []string, payload []byte, MaxHoldingSeconds uint32) error {
	msg := &OutboundMessage{
		Payload: payload,
		Dests: dests,
		MaxHoldingSeconds: MaxHoldingSeconds,
	}
	data, err := proto.Marshal(msg)
	err = c.conn.WriteMessage(websocket.BinaryMessage, data)
	return err
}

func (c *Client) Publish(topic string, payload []byte, MaxHoldingSeconds uint32) error {
	result, err := call("getsubscribers", map[string]interface{}{"topic": topic})
	if err != nil {
		return err
	}
	subscribers := result.([]interface{})
	if len(subscribers) == 0 {
		return nil
	}
	dests := make([]string, len(subscribers))
	for i, v := range subscribers {
		dests[i] = v.(string)
	}
	return c.Send(dests, payload, MaxHoldingSeconds)
}

func (c *Client) Close() {
	if !c.closed {
		c.closed = true
		_ = c.conn.Close()
	}
}