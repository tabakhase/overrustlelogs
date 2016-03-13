package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// errors
var (
	ErrTwitchLegacyAlreadyInChannel = errors.New("already in channel legacy")
	ErrTwitchLegacyNotInChannel     = errors.New("not in channel legacy")
	ErrChannelLegacyNotValid        = errors.New("not a valid channel legacy")
)

// TwitchLegacyChat twitch chat client
type TwitchLegacyChat struct {
	sync.RWMutex
	conn        *websocket.Conn
	dialer      websocket.Dialer
	headers     http.Header
	messages    map[string]chan *Message
	channels    []string
	writeLock   sync.Mutex
	joinHandler TwitchLegacyJoinHandler
	admins      map[string]bool
	tc          *TwitchChat
}

// TwitchLegacyJoinHandler called when joining channels
type TwitchLegacyJoinHandler func(string, chan *Message)

// NewTwitchLegacyChat new twitch chat client
func NewTwitchLegacyChat(j TwitchLegacyJoinHandler, tc *TwitchChat) *TwitchLegacyChat {
	c := &TwitchLegacyChat{
		dialer:      websocket.Dialer{HandshakeTimeout: SocketHandshakeTimeout},
		headers:     http.Header{"Origin": []string{"http://irc.twitch.tv"}},
		messages:    make(map[string]chan *Message, 0),
		channels:    make([]string, 0),
		joinHandler: j,
		admins:      make(map[string]bool, len(GetConfig().Twitch.Admins)),
		tc:          tc,
	}

	for _, u := range GetConfig().Twitch.Admins {
		c.admins[u] = true
	}

	d, err := ioutil.ReadFile(GetConfig().Twitch.ChannelListPath)
	if err != nil {
		log.Fatalf("unable to read channels legacy %s", err)
	}
	if err := json.Unmarshal(d, &c.channels); err != nil {
		log.Fatalf("unable to read channels legacy %s", err)
	}

	return c
}

// Connect open ws connection
func (c *TwitchLegacyChat) Connect() {
	var err error
	c.Lock()
	c.conn, _, err = c.dialer.Dial("ws://irc.twitch.tv:80/ws", c.headers)
	c.Unlock()
	if err != nil {
		log.Printf("error connecting to twitch ws legacy %s", err)
		c.reconnect()
	}

	c.send("PASS " + GetConfig().Twitch.OAuth)
	c.send("NICK " + GetConfig().Twitch.Nick)

	for _, ch := range c.channels {
		c.Join(ch, true)
	}
}

func (c *TwitchLegacyChat) reconnect() {
	if c.conn != nil {
		c.Lock()
		c.conn.Close()

		for ch, mc := range c.messages {
			close(mc)
			delete(c.messages, ch)
		}
		c.Unlock()
	}

	time.Sleep(SocketReconnectDelay)
	c.Connect()
}

// Run connect and start message read loop
func (c *TwitchLegacyChat) Run() {
	c.Connect()

	messagePattern := regexp.MustCompile(`:(.+)\!.+tmi\.twitch\.tv PRIVMSG #([a-z0-9_-]+) :(.+)`)

	for {
		err := c.conn.SetReadDeadline(time.Now().Add(SocketReadTimeout))
		if err != nil {
			c.reconnect()
			continue
		}

		c.RLock()
		_, msg, err := c.conn.ReadMessage()
		c.RUnlock()
		if err != nil {
			log.Printf("error reading message legacy %s", err)
			c.reconnect()
			continue
		}

		if strings.Index(string(msg), "PING") == 0 {
			c.send(strings.Replace(string(msg), "PING", "PONG", -1))
			continue
		}

		l := messagePattern.FindAllStringSubmatch(string(msg), -1)
		for _, v := range l {
			c.RLock()
			mc, ok := c.messages[strings.ToLower(v[2])]
			c.RUnlock()
			if !ok {
				continue
			}

			data := strings.TrimSpace(v[3])
			data = strings.Replace(data, "ACTION", "/me", -1)
			data = strings.Replace(data, "", "", -1)
			m := &Message{
				Command: "MSG",
				Nick:    v[1],
				Data:    data,
				Time:    time.Now().UTC(),
			}

			c.runCommand(v[2], m)

			select {
			case mc <- m:
			default:
			}
		}
	}
}

func (c *TwitchLegacyChat) runCommand(source string, m *Message) {
	if _, ok := c.admins[m.Nick]; ok && m.Command == "MSG" {
		d := strings.Split(m.Data, " ")
		ld := strings.Split(strings.ToLower(m.Data), " ")

		if strings.EqualFold(d[0], "!join") {
			if err := c.Join(ld[1], false); err == nil {
				if err := c.tc.Join(ld[1], false); err == nil {
					c.send("PRIVMSG #" + source + " :LoggingBC " + ld[1])
				} else {
					c.send("PRIVMSG #" + source + " :LoggingLegacy " + ld[1])
				}
			} else {
				if err == ErrChannelLegacyNotValid {
					c.send("PRIVMSG #" + source + " :Channel doesn't exist!")
				} else {
					c.send("PRIVMSG #" + source + " :Already logging " + ld[1])
				}
			}
		} else if strings.EqualFold(d[0], "!leave") {
			if err := c.Leave(ld[1]); err == nil {
				c.send("PRIVMSG #" + source + " :Leaving " + ld[1])
			} else {
				c.send("PRIVMSG #" + source + " :Not logging " + ld[1])
			}
		} else if strings.EqualFold(d[0], "!channels") {
			c.send("PRIVMSG #" + source + " :Logging " + strings.Join(c.channels, ", "))
		}
	}
}

func (c *TwitchLegacyChat) send(m string) {
	c.writeLock.Lock()
	c.RLock()
	err := c.conn.WriteMessage(websocket.TextMessage, []byte(m+"\r\n"))
	c.RUnlock()
	if err == nil {
		time.Sleep(SocketWriteDebounce)
	}
	c.writeLock.Unlock()
	if err != nil {
		log.Printf("error sending message legacy %s", err)
		c.reconnect()
	}
}

// Join channel
func (c *TwitchLegacyChat) Join(ch string, init bool) error {
	ch = strings.ToLower(ch)
	if !channelLegacyExists(ch) {
		return ErrChannelLegacyNotValid
	}
	c.Lock()
	_, ok := c.messages[ch]
	if !ok {
		c.messages[ch] = make(chan *Message, MessageBufferSize)
	}
	c.Unlock()
	if ok {
		return ErrTwitchLegacyAlreadyInChannel
	}
	c.send("JOIN #" + ch)
	c.Lock()
	if messages, ok := c.messages[ch]; ok {
		go c.joinHandler(ch, messages)
	}
	c.Unlock()
	if init {
		return nil
	}
	return c.saveChannels()
}

// Leave channel
func (c *TwitchLegacyChat) Leave(ch string) error {
	ch = strings.ToLower(ch)
	c.Lock()
	_, ok := c.messages[ch]
	c.Unlock()
	if !ok {
		return ErrTwitchLegacyNotInChannel
	}
	c.send("PART #" + ch)
	c.Lock()
	delete(c.messages, ch)
	c.Unlock()
	return c.saveChannels()
}

// channelExists
func channelLegacyExists(ch string) bool {
	res, err := http.Head("https://api.twitch.tv/kraken/users/" + ch)
	if err != nil {
		return false
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false
	}
	return true
}

func (c *TwitchLegacyChat) saveChannels() error {
	c.Lock()
	defer c.Unlock()
	c.channels = []string{}
	for ch := range c.messages {
		c.channels = append(c.channels, ch)
	}
	f, err := os.Create(GetConfig().Twitch.ChannelListPath)
	if err != nil {
		log.Printf("error saving channel list legacy %s", err)
		return err
	}
	data, err := json.Marshal(c.channels)
	if err != nil {
		log.Printf("error saving channel list legacy %s", err)
		return err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "\t"); err != nil {
		log.Printf("error saving channel list legacy %s", err)
	}
	f.Write(buf.Bytes())
	f.Close()
	return nil
}
