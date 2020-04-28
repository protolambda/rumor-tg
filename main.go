package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
	"html"
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
	var adminUsername string
	mainCmd := cobra.Command{
		Use:   "rumor-tg",
		Short: "Start Rumor telegram bot",
		Run: func(cmd *cobra.Command, args []string) {
			log := logrus.New()
			log.SetOutput(os.Stdout)
			log.SetLevel(logrus.TraceLevel)
			log.SetFormatter(&logrus.TextFormatter{DisableTimestamp: true})

			startBot(log, adminUsername, tgToken, extName, port, socketType, socketPath)
		},
	}
	mainCmd.Flags().StringVar(&tgToken, "token", "", "Telegram bot token. Ask the Botfather: https://core.telegram.org/bots#6-botfather")
	mainCmd.Flags().StringVar(&extName, "ext", "foobar.ngrok.io:80", "IP or domain name pointing to this bot, used to register the webhook. Use ngrok for local debugging.")
	mainCmd.Flags().Uint16Var(&port, "port", 8443, "Port to use locally for the webhook")
	mainCmd.Flags().StringVar(&socketType, "stype", "unix", "Type of socket to use to make connections to Rumor. 'unix' or 'tcp'")
	mainCmd.Flags().StringVar(&socketPath, "spath", "example.sock", "Path/address to socket to make connections to Rumor")
	mainCmd.Flags().StringVar(&adminUsername, "admin", "protolambda", "Telegram username of admin with special permissions")

	if err := mainCmd.Execute(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to run Rumor telegram bot: %v", err)
		os.Exit(1)
	} else {
		os.Exit(0)
	}
}

func startBot(log logrus.FieldLogger, adminUsername string, tgToken string, extName string, port uint16, rumorSocketType string, rumorSocketPath string) {
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

	// Clear old updates, forget about backlog after restart
	time.Sleep(time.Second * 3)
	updates.Clear()

	rbot := &RumorBot{
		adminUser: adminUsername,
		whitelistedUsers: make(map[string]struct{}),
		openSessions: make(map[SessionID]*Session),
		socketType: rumorSocketType,
		socketPath: rumorSocketPath,
		log:    log,
		BotAPI: bot,
		ctx:    ctx,
	}
	rbot.ListenUpdates(updates)
}

type EntryMsgTracker struct {
	tgID int
	callID string
	content string
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
	lastEntryMsg  EntryMsgTracker
	// don't edit last message at the same time
	lastEntryMsgLock sync.Mutex
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

func (s *Session) Start(nextTryAfter time.Duration) {
	s.connLock.Lock()
	defer s.connLock.Unlock()
	if s.conn != nil {
		// already started
		return
	}
	conn, err := net.Dial(s.socketType, s.socketPath)
	if err != nil {
		s.ChatMsg("failed to reach Rumor instance, retry after "+nextTryAfter.String())
		s.log.WithError(err).Error(conn)
		// retry after a while
		time.Sleep(nextTryAfter)
		go s.Start(nextTryAfter * 150 / 100)
		return
	}
	s.conn = conn
	s.ChatMsg("connected to Rumor")
	s.log.Infof("session %s connected to Rumor: %s/%s", s.id, s.socketPath, s.socketType)
	go func() {
		nextLine := asLineReader(conn)
		for {
			line, err := nextLine()
			if err != nil {
				if s.stopped {
					return
				}
				s.log.Errorf("got error when reading line, resetting connection: %v", err)
				conn := s.conn
				if conn != nil {
					_ = conn.Close()
				}
				s.conn = nil
				go s.Start(nextTryAfter)
				return
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

func (s *Session) Stop() {
	s.connLock.Lock()
	defer s.connLock.Unlock()
	s.log.Info("Stopping session")
	s.stopped = true
	err := s.conn.Close()
	if err != nil {
		s.log.Error(err)
	}
	if s.LocatedIn.IsPrivate() {
		s.ChatMsg("Stopped session.")
	} else {
		s.ChatMsg(fmt.Sprintf("Stopped session of @%s.", s.StartedBy.UserName))
	}
	s.log.Info("stopped session")
}

func (s *Session) ChatMsg(msg string) (id int, err error) {
	tmsg := tgbotapi.NewMessage(s.LocatedIn.ID, msg)
	tmsg.ParseMode = "markdown"
	m, err := s.bot.Send(tmsg)
	if err != nil {
		s.log.Error(err)
	}
	return m.MessageID, err
}

func (s *Session) ChatMarkdown(msg string) (id int, err error) {
	tmsg := tgbotapi.NewMessage(s.LocatedIn.ID, msg)
	tmsg.ParseMode = "markdown"
	m, err := s.bot.Send(tmsg)
	if err != nil {
		s.log.Error(err)
	}
	return m.MessageID, err
}

func (s *Session) ChatMsgHtml(msg string) (id int, err error) {
	tmsg := tgbotapi.NewMessage(s.LocatedIn.ID, msg)
	tmsg.ParseMode = "html"
	m, err := s.bot.Send(tmsg)
	if err != nil {
		s.log.Error(err)
	}
	return m.MessageID, err
}

func logLvlToEmoji(lvl string) string {
	switch strings.ToLower(lvl) {
	case "panic":
		return "🚨"
	case "fatal":
		return "☠️"
	case "error":
		return "🔥"
	case "warn", "warning":
		return "⚠️"
	case "info":
		return "ℹ️"
	case "debug":
		return "🔍"
	case "trace":
		return "🕵️"
	default:
		return "❓"
	}
}

func (s *Session) ChatEntry(entry map[string]interface{}) {
	callID, _ := entry["call_id"]
	delete(entry, "call_id")
	actor, _ := entry["actor"]
	delete(entry, "actor")
	lvl, _ := entry["level"]
	delete(entry, "level")
	msg, _ := entry["msg"]
	delete(entry, "msg")
	//time, _ := entry["time"]
	delete(entry, "time")

	var buf strings.Builder

	writeContents := func() {
		if msg != nil {
			msgStr := msg.(string)
			msgStr = strings.TrimRight(msgStr, "\n")
			buf.WriteString("\n<pre>")
			buf.WriteString(html.EscapeString(msgStr))
			buf.WriteString("</pre>")
		}

		if len(entry) > 0 {
			for k, v := range entry {
				buf.WriteString("<strong>")
				buf.WriteString(html.EscapeString(k))
				buf.WriteString("</strong>")
				buf.WriteString(": ")
				snip := false
				buf.WriteString("<code>")
				vStr, ok := v.(string)
				if !ok {
					vStrB, err := yaml.Marshal(v)
					if err != nil {
						vStr = "???"
					}
					vStr = string(vStrB)
				}
				// put multi-line strings apart from the key
				if strings.Contains(vStr, "\n") {
					vStr = "\n" + vStr
				}
				if len(vStr) > 300 {
					vStr = vStr[:300]
					snip = true
				}
				buf.WriteString(html.EscapeString(vStr))
				buf.WriteString("</code>")
				if snip {
					buf.WriteString("...")
				}
				buf.WriteRune('\n')
			}
		}
	}

	// Append to last message if it's part of the same command
	s.lastEntryMsgLock.Lock()
	defer s.lastEntryMsgLock.Unlock()
	last := s.lastEntryMsg
	if callID != nil && last.callID == callID.(string) {
		buf.WriteString(last.content)
		if _, ok := entry["@success"]; ok {
			buf.WriteString("\n✅ completed")
		} else {
			writeContents()
		}
		last.content = buf.String()
		// dont't proceed if the content is large
		if len(last.content) < 1000 {
			s.lastEntryMsg = last
			editTxt := tgbotapi.NewEditMessageText(s.LocatedIn.ID, last.tgID, last.content)
			editTxt.ParseMode = "html"
			s.bot.Send(editTxt)
			return
		} else {
			// Stop next message from appending to this large one
			buf.Reset()
		}
	}

	if _, ok := entry["@success"]; ok {
		s.ChatMsgHtml(fmt.Sprintf("✅ completed call <code>%s</code> by actor <code>%s</code>",
			html.EscapeString(callID.(string)), html.EscapeString(actor.(string))))
		return
	}

	if msg != nil {
		buf.WriteString(logLvlToEmoji(lvl.(string)))
	}
	if callID != nil {
		buf.WriteString(" <code>")
		buf.WriteString(html.EscapeString(callID.(string)))
		buf.WriteString("</code>")
	}
	if actor != nil {
		buf.WriteString(" <strong>")
		buf.WriteString(html.EscapeString(actor.(string)))
		buf.WriteString("</strong>")
	}
	writeContents()
	content := buf.String()
	m, err := s.ChatMsgHtml(content)
	if err != nil {
		// already logged it
		return
	}
	if callID != nil {
		s.lastEntryMsg = EntryMsgTracker{
			tgID: m,
			callID: callID.(string),
			content: content,
		}
	}
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
	adminUser string
	whitelistLock sync.Mutex
	whitelistedUsers map[string]struct{}

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
			log:        b.log.WithFields(logrus.Fields{"session_id": id, "started_by": startedBy.UserName, "located_in": locatedIn.Title}),
		}
		b.openSessions[id] = s
		go s.Start(time.Second * 5)
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

func (b *RumorBot) stopSession(startedById int, locatedInId int64) (ok bool) {
	b.sessionsLock.Lock()
	defer b.sessionsLock.Unlock()
	id := NewSessionID(startedById, locatedInId)
	s, ok := b.openSessions[id]
	if !ok {
		return false
	}
	delete(b.openSessions, id)
	s.Stop()
	return true
}

func (b *RumorBot) allowUsers(users []string) string {
	b.whitelistLock.Lock()
	defer b.whitelistLock.Unlock()
	for _, p := range users {
		b.whitelistedUsers[p] = struct{}{}
	}
	return fmt.Sprintf("Allowing users %s to use rumor bot", strings.Join(users, ", "))
}

func (b *RumorBot) denyUsers(users []string) string {
	b.whitelistLock.Lock()
	defer b.whitelistLock.Unlock()
	for _, p := range users {
		delete(b.whitelistedUsers, p)
	}
	return fmt.Sprintf("Denying users %s to use rumor bot", strings.Join(users, ", "))
}

func (b *RumorBot) listUsers() string {
	b.whitelistLock.Lock()
	defer b.whitelistLock.Unlock()
	usernames := make([]string, 0)
	for user := range b.whitelistedUsers {
		usernames = append(usernames, user)
	}
	return fmt.Sprintf("Allowed users: %s", strings.Join(usernames, ", "))
}

func (b *RumorBot) processUpdate(update tgbotapi.Update) {
	b.log.Debugf("%+v\n", update)

	sendChat := func(msg string) {
		b.Send(tgbotapi.NewMessage(update.Message.Chat.ID, msg))
	}
	if update.Message == nil {
		return
	}
	if update.Message.IsCommand() {
		if withAt := update.Message.CommandWithAt(); strings.Contains(withAt, "@") && !b.IsMessageToMe(*update.Message) {
			// Ignore message if it was intended for some other bot
			return
		}
		if _, ok := b.whitelistedUsers[update.Message.From.UserName]; !ok && update.Message.From.UserName != b.adminUser {
			sendChat(fmt.Sprintf("Sorry @%s, this bot is not allowed to serve you. Ask @%s", update.Message.From.UserName, b.adminUser))
			return
		}
		switch update.Message.Command() {
		case "start":
			s := b.createSession(update.Message.From, update.Message.Chat)
			s.ChatMsg("Started! Send me a command with /r")
		case "stop":
			ok := b.stopSession(update.Message.From.ID, update.Message.Chat.ID)
			if !ok {
				sendChat(fmt.Sprintf("Could not find open session for @%s", update.Message.From.UserName))
				return
			}
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
		case "access":
			if update.Message.From.UserName != b.adminUser {
				sendChat(fmt.Sprintf("Only %s has access to `/allow`", b.adminUser))
				return
			}
			cmdArgs := update.Message.CommandArguments()
			r := regexp.MustCompile("[^\\s]+")
			parts := r.FindAllString(cmdArgs, -1)
			if len(parts) >= 1 {
				switch parts[0] {
				case "add":
					sendChat(b.allowUsers(parts[1:]))
				case "rm":
					sendChat(b.denyUsers(parts[1:]))
				case "list":
					sendChat(b.listUsers())
				default:
					sendChat("Invalid input, try: /access list or /access add|rm usernames... (space separated)")
				}
			} else {
				sendChat("Need at least 1 argument")
			}
		case "c":
			if update.Message.From.UserName != b.adminUser {
				sendChat(fmt.Sprintf("Only %s has access to `/c`", b.adminUser))
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
