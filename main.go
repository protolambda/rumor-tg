package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-telegram-bot-api/telegram-bot-api"
)

func main() {
	var tgToken string
	var extName string
	var port uint16
	var socketType string
	var socketPath string
	mainCmd := cobra.Command{
		Use:   "rumor-tg",
		Short: "Start Rumor telegram bot",
		Run: func(cmd *cobra.Command, args []string) {
			log := logrus.New()
			log.SetOutput(os.Stdout)
			log.SetLevel(logrus.TraceLevel)
			log.SetFormatter(&logrus.TextFormatter{DisableTimestamp: true})

			startBot(log, tgToken, extName, port, socketType, socketPath)
		},
	}
	mainCmd.Flags().StringVar(&tgToken, "token", "", "Telegram bot token. Ask the Botfather: https://core.telegram.org/bots#6-botfather")
	mainCmd.Flags().StringVar(&extName, "ext", "foobar.ngrok.io:80", "IP or domain name pointing to this bot, used to register the webhook. Use ngrok for local debugging.")
	mainCmd.Flags().Uint16Var(&port, "port", 8443, "Port to use locally for the webhook")
	mainCmd.Flags().StringVar(&socketType, "stype", "unix", "Type of socket to use to make connections to Rumor. 'unix' or 'tcp'")
	mainCmd.Flags().StringVar(&socketPath, "spath", "example.sock", "Path/address to socket to make connections to Rumor")

	if err := mainCmd.Execute(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to run Rumor telegram bot: %v", err)
		os.Exit(1)
	} else {
		os.Exit(0)
	}
}

func startBot(log logrus.FieldLogger, tgToken string, extName string, port uint16, rumorSocketType string, rumorSocketPath string) {
	bot, err := tgbotapi.NewBotAPI(tgToken)
	if err != nil {
		log.Fatal(err)
	}

	bot.Debug = true

	log.Infof("Authorized on account %s", bot.Self.UserName)

	hostName := fmt.Sprintf("https://%s/%s", extName, bot.Token)
	log.Infof("host: %s", hostName)
	_, err = bot.SetWebhook(tgbotapi.NewWebhook(hostName))
	if err != nil {
		log.Fatal(err)
	}
	info, err := bot.GetWebhookInfo()
	if err != nil {
		log.Fatal(err)
	}
	if info.LastErrorDate != 0 {
		log.Infof("Telegram callback failed: %s", info.LastErrorMessage)
	}
	updates := bot.ListenForWebhook("/" + bot.Token)
	go http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", port), nil)

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sig
		log.Infof("Caught signal %s: shutting down.", sig)
		cancel()
	}()

	rbot := &RumorBot{
		openSessions: make(map[SessionID]*Session),
		socketType: rumorSocketType,
		socketPath: rumorSocketPath,
		log:    log,
		BotAPI: bot,
		ctx:    ctx,
	}
	rbot.ListenUpdates(updates)
}

type Session struct {
	id         SessionID
	bot        *tgbotapi.BotAPI
	socketType string
	socketPath string
	conn       net.Conn
	connLock   sync.Mutex
	StartedBy  *tgbotapi.User
	LocatedIn  *tgbotapi.Chat
	stopped    bool
	log        logrus.FieldLogger
}

func asLineReader(r io.Reader) func() (s string, err error) {
	sc := bufio.NewScanner(r)
	return func() (s string, err error) {
		hasMore := sc.Scan()
		text := sc.Text()
		err = sc.Err()
		if err == nil && !hasMore {
			err = io.EOF
		}
		return text, err
	}
}

func (s *Session) Start() {
	s.connLock.Lock()
	defer s.connLock.Unlock()
	if s.conn != nil {
		// already started
		return
	}
	conn, err := net.Dial(s.socketType, s.socketPath)
	if err != nil {
		s.ChatMsg("failed to reach Rumor instance")
		s.log.WithError(err).Error(conn)
		return
	}
	s.conn = conn
	s.ChatMsg("connected to Rumor")
	s.log.Info("session %s connected to Rumor: %s/%s", s.id, s.socketPath, s.socketType)
	go func() {
		nextLine := asLineReader(conn)
		for {
			line, err := nextLine()
			if err != nil {
				if s.stopped {
					return
				}
				s.log.Errorf("got error when reading line, resetting connection: %v", err)
				_ = s.conn.Close()
				s.conn = nil
				// don't open connections too quick in case of unforeseen problems
				time.Sleep(time.Second * 5)
				go s.Start()
				continue
			}
			var data map[string]interface{}
			if err := json.Unmarshal([]byte(line), &data); err != nil {
				s.log.WithField("line", line).Warn("Invalid output from Rumor")
				continue
			}
			s.ChatEntry(data)
		}
	}()
}

// TODO command to explitly kill existing session and reconnect to rumor

func (s *Session) ChatMsg(msg string) {
	tmsg := tgbotapi.NewMessage(s.LocatedIn.ID, msg)
	if _, err := s.bot.Send(tmsg); err != nil {
		s.log.Error(err)
	}
}

func (s *Session) ChatEntry(entry map[string]interface{}) {
	// TODO: format msg nicely
	dat, _ := json.Marshal(entry)
	s.ChatMsg(string(dat))
}

// TODO also option for log level of call
// Send commands to rumor.
func (s *Session) SendCmds(actor string, owner string, cmdStr string) {
	cmdLines := strings.Split(cmdStr, "\n")
	var buf strings.Builder
	for _, cmdLine := range cmdLines {
		cmdLine = strings.TrimSpace(cmdLine)
		if cmdLine == "" {
			continue
		}
		buf.WriteString(owner)
		buf.WriteString("$ ")
		buf.WriteString(actor)
		buf.WriteString(": ")
		buf.WriteString(cmdLine)
		buf.WriteRune('\n')
	}
	data := []byte(buf.String())
	if len(data) > 0 {
		_, err := s.conn.Write(data)
		if err != nil {
			s.ChatMsg("failed to send cmd to rumor")
			s.log.Error(err)
		}
	}
}

type SessionID string

func NewSessionID(fromID int, chatID int64) SessionID {
	return SessionID(fmt.Sprintf("user%d_chat%d", fromID, chatID))
}

type RumorBot struct {
	openSessions map[SessionID]*Session
	sessionsLock sync.RWMutex

	socketType string
	socketPath string

	ctx context.Context
	log logrus.FieldLogger
	*tgbotapi.BotAPI
}

func (b *RumorBot) ListenUpdates(updates tgbotapi.UpdatesChannel) {
	for {
		select {
		case update := <-updates:
			b.processUpdate(update)
		case <-b.ctx.Done():
			b.log.Info("Exiting bot...")
			return
		}
	}
}

func (b *RumorBot) createSession(startedBy *tgbotapi.User, locatedIn *tgbotapi.Chat) *Session {
	b.sessionsLock.Lock()
	defer b.sessionsLock.Unlock()
	id := NewSessionID(startedBy.ID, locatedIn.ID)
	s, ok := b.openSessions[id]
	if ok {
		return s
	} else {
		s = &Session{
			id:         id,
			bot:        b.BotAPI,
			socketType: b.socketType,
			socketPath: b.socketPath,
			StartedBy:  startedBy,
			LocatedIn:  locatedIn,
			stopped:    false,
			log:        b.log.WithField("session_id", id),
		}
		b.openSessions[id] = s
		go s.Start()
		return s
	}
}

// TODO command to stop session


func (b *RumorBot) getSession(startedById int, locatedInId int64) (s *Session, ok bool) {
	b.sessionsLock.RLock()
	defer b.sessionsLock.RUnlock()
	id := NewSessionID(startedById, locatedInId)
	s, ok = b.openSessions[id]
	return
}

func (b *RumorBot) processUpdate(update tgbotapi.Update) {
	b.log.Debugf("%+v\n", update)

	sendChat := func(msg string) {
		b.Send(tgbotapi.NewMessage(update.Message.Chat.ID, msg))
	}
	if update.Message.IsCommand() {

		switch update.Message.Command() {
		case "start":
			s := b.createSession(update.Message.From, update.Message.Chat)
			s.ChatMsg("Started! Send me a command with /s")
		case "help":
			sendChat(fmt.Sprintf("Start Rumor with /start@%s, write Rumor commands with /r", b.Self.UserName))
		case "r", "rumor":
			s, ok := b.getSession(update.Message.From.ID, update.Message.Chat.ID)
			if !ok {
				sendChat(fmt.Sprintf("Could not find open session for @%s. Start with /start@%s", update.Message.From.UserName, b.Self.UserName))
				return
			}
			actor := update.Message.From.UserName
			owner := update.Message.From.UserName
			cmds := update.Message.CommandArguments()
			s.SendCmds(actor, owner, cmds)
		case "c":
			if update.Message.From.UserName != "protolambda" {
				sendChat("Only protolambda has access to `/c`")
				return
			}
			s, ok := b.getSession(update.Message.From.ID, update.Message.Chat.ID)
			if !ok {
				sendChat(fmt.Sprintf("Could not find open session for @%s. Start with /start@%s", update.Message.From.UserName, b.Self.UserName))
				return
			}
			inputStr := update.Message.CommandArguments()
			r := regexp.MustCompile("[^\\s]+")
			parts := r.FindAllString(inputStr, 3)
			if len(parts) < 3 {
				s.ChatMsg(fmt.Sprintf("@%s fix your input, format is: `/c <actor> <owner> <actual command>`", update.Message.From.UserName))
				return
			}
			actor := parts[0]
			owner := parts[1]
			cmds := parts[2]
			s.SendCmds(actor, owner, cmds)
		default:
			sendChat(fmt.Sprintf("unknown command, sorry @%s", update.Message.From.UserName))
		}
	}
}
