package tenderduty

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/PagerDuty/go-pagerduty"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type alertMsg struct {
	pd   bool
	disc bool
	tg   bool

	severity string
	resolved bool
	chain    string
	message  string
	uniqueId string
	key      string

	tgChannel  string
	tgKey      string
	tgMentions string

	discHook     string
	discMentions string
}

type notifyDest uint8

const (
	pd notifyDest = iota
	tg
	di
)

var (
	sentPdAlarms = make(map[string]bool)
	sentTgAlarms = make(map[string]bool)
	sentDAlarms  = make(map[string]bool)
	notifyMux    sync.Mutex
)

func shouldNotify(msg *alertMsg, dest notifyDest) bool {
	notifyMux.Lock()
	defer notifyMux.Unlock()
	var whichMap map[string]bool
	var service string
	switch dest {
	case pd:
		whichMap = sentPdAlarms
		service = "PagerDuty"
	case tg:
		whichMap = sentTgAlarms
		service = "Telegram"
	case di:
		whichMap = sentDAlarms
		service = "Discord"
	}
	if whichMap[msg.message] && !msg.resolved {
		// already sent this alert
		return false
	} else if whichMap[msg.message] && msg.resolved {
		// alarm is cleared
		delete(whichMap, msg.message)
		l(fmt.Sprintf("💜 Resolved     alarm on %s (%s) - notifying %s", msg.chain, msg.message, service))
		return true
	}
	whichMap[msg.message] = true
	l(fmt.Sprintf("🚨 ALERT        new alarm on %s (%s) - notifying %s", msg.chain, msg.message, service))
	return true
}

func notifyDiscord(msg *alertMsg) (err error) {
	if !msg.disc {
		return nil
	}
	if !shouldNotify(msg, di) {
		return nil
	}
	discPost := buildDiscordMessage(msg)
	client := &http.Client{}
	data, err := json.MarshalIndent(discPost, "", "  ")
	if err != nil {
		l("notify discord:", err)
		return err
	}

	req, err := http.NewRequest("POST", msg.discHook, bytes.NewBuffer(data))
	if err != nil {
		l("notify discord:", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		l("notify discord:", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 204 {
		log.Println(resp)
		//if resp.Body != nil {
		//	b, _ := ioutil.ReadAll(resp.Body)
		//	_ = resp.Body.Close()
		//	fmt.Println(string(b))
		//}
		l("notify discord:", err)
		return err
	}
	return nil
}

type DiscordMessage struct {
	Username  string         `json:"username,omitempty"`
	AvatarUrl string         `json:"avatar_url,omitempty"`
	Content   string         `json:"content"`
	Embeds    []DiscordEmbed `json:"embeds,omitempty"`
}

type DiscordEmbed struct {
	Title       string `json:"title,omitempty"`
	Url         string `json:"url,omitempty"`
	Description string `json:"description"`
	Color       uint   `json:"color"`
}

func buildDiscordMessage(msg *alertMsg) *DiscordMessage {
	prefix := "🚨 ALERT: "
	if msg.resolved {
		prefix = "💜 Resolved: "
	}
	return &DiscordMessage{
		Username: "tenderuty",
		Content:  prefix + msg.chain,
		Embeds: []DiscordEmbed{{
			Description: msg.message,
		}},
	}
}

func notifyTg(msg *alertMsg) (err error) {
	if !msg.tg {
		return nil
	}
	if !shouldNotify(msg, tg) {
		return nil
	}
	//tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	bot, err := tgbotapi.NewBotAPI(msg.tgKey)
	if err != nil {
		l("notify telegram:", err)
		return
	}

	prefix := "🚨 ALERT: "
	if msg.resolved {
		prefix = "💜 Resolved: "
	}

	mc := tgbotapi.NewMessageToChannel(msg.tgChannel, fmt.Sprintf("%s: %s - %s", msg.chain, prefix, msg.message))
	//mc.ParseMode = "html"
	_, err = bot.Send(mc)
	if err != nil {
		l("telegram send:", err)
	}
	return err
}

func notifyPagerduty(msg *alertMsg) (err error) {
	if !msg.pd {
		return nil
	}
	if !shouldNotify(msg, pd) {
		return nil
	}
	// key from the example, don't spam their api
	if msg.key == "aaaaaaaaaaaabbbbbbbbbbbbbcccccccccccc" {
		l("invalid pagerduty key")
		return
	}
	action := "trigger"
	if msg.resolved {
		action = "resolve"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = pagerduty.ManageEventWithContext(ctx, pagerduty.V2Event{
		RoutingKey: msg.key,
		Action:     action,
		DedupKey:   msg.uniqueId,
		Payload: &pagerduty.V2Payload{
			Summary:  msg.message,
			Source:   msg.uniqueId,
			Severity: msg.severity,
		},
	})
	return
}

var (
	currentAlarms    = make(map[string]map[string]bool)
	currentAlarmsMux = sync.RWMutex{}
)

func getAlarms(chain string) string {
	currentAlarmsMux.RLock()
	defer currentAlarmsMux.RUnlock()
	// don't show this info if the logs are disabled on the dashboard, potentially sensitive info could be leaked.
	//if td.HideLogs || currentAlarms[chain] == nil {
	if td.HideLogs || currentAlarms[chain] == nil {
		return ""
	}
	result := ""
	for k := range currentAlarms[chain] {
		result += "🚨 " + k + "\n"
	}
	return result
}

// alert creates a universal alert and pushes it to the alertChan to be delivered to appropriate services
func (c *Config) alert(chainName, message, severity string, resolved bool, id *string) {
	uniq := c.Chains[chainName].ValAddress
	if id != nil {
		uniq = *id
	}
	c.chainsMux.RLock()
	a := &alertMsg{
		pd:           c.Pagerduty.Enabled && c.Chains[chainName].Alerts.PagerdutyAlerts,
		disc:         c.Discord.Enabled && c.Chains[chainName].Alerts.DiscordAlerts,
		tg:           c.Telegram.Enabled && c.Chains[chainName].Alerts.TelegramAlerts,
		severity:     severity,
		resolved:     resolved,
		chain:        chainName,
		message:      message,
		uniqueId:     uniq,
		key:          c.Pagerduty.ApiKey,
		tgChannel:    c.Telegram.Channel,
		tgKey:        c.Telegram.ApiKey,
		tgMentions:   strings.Join(c.Telegram.Mentions, " "),
		discHook:     c.Discord.Webhook,
		discMentions: strings.Join(c.Discord.Mentions, " "),
	}
	c.alertChan <- a
	c.chainsMux.RUnlock()
	currentAlarmsMux.Lock()
	defer currentAlarmsMux.Unlock()
	if currentAlarms[chainName] == nil {
		currentAlarms[chainName] = make(map[string]bool)
	}
	if resolved && currentAlarms[chainName][message] {
		delete(currentAlarms[chainName], message)
		return
	} else if resolved {
		return
	}
	currentAlarms[chainName][message] = true
}

// watch handles monitoring for missed blocks, stalled chain, node downtime
// and also updates a few prometheus stats
func (cc *ChainConfig) watch() {
	var missedAlarm, pctAlarm, noNodes bool
	nodeAlarms := make(map[string]bool)

	// wait until we have a moniker:
	for {
		if cc.valInfo == nil || cc.valInfo.Moniker == "not connected" {
			time.Sleep(time.Second)
			if cc.Alerts.AlertIfNoServers && !noNodes && cc.noNodes {
				noNodes = true
				td.alert(
					cc.name,
					fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId),
					"critical",
					false,
					&cc.valInfo.Valcons,
				)
			}
			continue
		}
		break
	}
	// initial stat creation for nodes, we only update again if the node is positive
	if td.Prom {
		for _, node := range cc.Nodes {
			td.statsChan <- cc.mkUpdate(metricNodeDownSeconds, 0, node.Url)
		}
	}

	for {
		time.Sleep(2 * time.Second)

		// alert if we can't monitor
		if cc.Alerts.AlertIfNoServers && !noNodes && cc.noNodes {
			noNodes = true
			td.alert(
				cc.name,
				fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId),
				"critical",
				false,
				&cc.valInfo.Valcons,
			)
		} else if cc.Alerts.AlertIfNoServers && noNodes && !cc.noNodes {
			noNodes = false
			td.alert(
				cc.name,
				fmt.Sprintf("no RPC endpoints are working for %s", cc.ChainId),
				"critical",
				true,
				&cc.valInfo.Valcons,
			)
		}

		if cc.Alerts.StalledAlerts && !cc.lastBlockAlarm && !cc.lastBlockTime.IsZero() &&
			cc.lastBlockTime.Before(time.Now().Add(time.Duration(-cc.Alerts.Stalled)*time.Minute)) {

			// chain is stalled send an alert!
			cc.lastBlockAlarm = true
			td.alert(
				cc.name,
				fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", cc.ChainId, cc.Alerts.Stalled),
				"critical",
				false,
				&cc.valInfo.Valcons,
			)
		} else if cc.Alerts.StalledAlerts && cc.lastBlockAlarm && cc.lastBlockTime.IsZero() {
			cc.lastBlockAlarm = false
			td.alert(
				cc.name,
				fmt.Sprintf("stalled: have not seen a new block on %s in %d minutes", cc.ChainId, cc.Alerts.Stalled),
				"critical",
				true,
				&cc.valInfo.Valcons,
			)
		}

		// consecutive missed block alarms:
		if !missedAlarm && cc.Alerts.ConsecutiveAlerts && int(cc.statConsecutiveMiss) >= cc.Alerts.ConsecutiveMissed {
			// alert on missed block counter!
			missedAlarm = true
			cc.activeAlerts += 1
			id := cc.valInfo.Valcons + "consecutive"
			td.alert(
				cc.name,
				fmt.Sprintf("%s has missed %d blocks on %s", cc.valInfo.Moniker, cc.Alerts.ConsecutiveMissed, cc.ChainId),
				"critical",
				false,
				&id,
			)
		} else if missedAlarm && int(cc.statConsecutiveMiss) < cc.Alerts.ConsecutiveMissed {
			// clear the alert
			missedAlarm = false
			cc.activeAlerts -= 1
			id := cc.valInfo.Valcons + "consecutive"
			td.alert(
				cc.name,
				fmt.Sprintf("%s has missed %d blocks on %s", cc.valInfo.Moniker, cc.Alerts.ConsecutiveMissed, cc.ChainId),
				"critical",
				true,
				&id,
			)
		}

		// window percentage missed block alarms
		//fmt.Println(100*float64(cc.valInfo.Missed)/float64(cc.valInfo.Window), float64(cc.Alerts.Window))
		if cc.Alerts.PercentageAlerts && !pctAlarm && 100*float64(cc.valInfo.Missed)/float64(cc.valInfo.Window) > float64(cc.Alerts.Window) {
			// alert on missed block counter!
			pctAlarm = true
			id := cc.valInfo.Valcons + "percent"
			cc.activeAlerts += 1
			td.alert(
				cc.name,
				fmt.Sprintf("%s has missed > %d%% of the slashing window's blocks on %s", cc.valInfo.Moniker, cc.Alerts.Window, cc.ChainId),
				"critical",
				false,
				&id,
			)
		} else if cc.Alerts.PercentageAlerts && pctAlarm && 100*float64(cc.valInfo.Missed)/float64(cc.valInfo.Window) < float64(cc.Alerts.Window) {
			// clear the alert
			pctAlarm = false
			id := cc.valInfo.Valcons + "percent"
			cc.activeAlerts -= 1
			td.alert(
				cc.name,
				fmt.Sprintf("%s has missed > %d%% of the slashing window's blocks on %s", cc.valInfo.Moniker, cc.Alerts.Window, cc.ChainId),
				"critical",
				false,
				&id,
			)
		}

		// node down alarms
		for _, node := range cc.Nodes {
			// window percentage missed block alarms
			if node.AlertIfDown && node.down && !nodeAlarms[node.Url] && !node.downSince.IsZero() && time.Now().Sub(node.downSince).Minutes() > float64(td.NodeDownMin) {
				// alert on dead node
				cc.activeAlerts += 1
				nodeAlarms[node.Url] = true
				td.alert(
					cc.name,
					fmt.Sprintf("RPC node %s has been down for > %d minutes on %s", node.Url, td.NodeDownMin, cc.ChainId),
					"critical",
					false,
					&node.Url,
				)
			} else if nodeAlarms[node.Url] && node.downSince.IsZero() {
				// clear the alert
				cc.activeAlerts -= 1
				nodeAlarms[node.Url] = false
				td.alert(
					cc.name,
					fmt.Sprintf("RPC node %s has been down for > %d minutes on %s", node.Url, td.NodeDownMin, cc.ChainId),
					"critical",
					false,
					&node.Url,
				)
			}
		}

		if td.Prom {
			// raw block timer, ignoring finalized state
			td.statsChan <- cc.mkUpdate(metricLastBlockSecondsNotFinal, time.Now().Sub(cc.lastBlockTime).Seconds(), "")
			// update node-down times for prometheus
			for _, node := range cc.Nodes {
				if node.down && !node.downSince.IsZero() {
					td.statsChan <- cc.mkUpdate(metricNodeDownSeconds, time.Now().Sub(node.downSince).Seconds(), node.Url)
				}
			}
		}
	}
}