/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is Tamás Gulácsi.
# Portions created by the Initial Developer are Copyright (C) 2013
# the Initial Developer. All Rights Reserved.
#
# ***** END LICENSE BLOCK *****/

package email

import (
	"github.com/mozilla-services/heka/pipeline"
	"github.com/tgulacsi/heka-plugins/utils"

	"bytes"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout is the default timeout
var DefaultTimeout = 30 * time.Second

// EmailOutput holds the config values for the Email Output plugin
type EmailOutput struct {
	From      string
	To        []string
	hostport  string
	auth      smtp.Auth
	byHost    map[string][]string
	tlsConfig *tls.Config
}

// EmailOutputConfig is for reading the configuration file
type EmailOutputConfig struct {
	Address     string   `toml:"address"`
	Username    string   `toml:"username"`
	Password    string   `toml:"password"`
	From        string   `toml:"from"`
	To          []string `toml:"to"`
	NoCertCheck bool     `toml:"no_cert_check"`
}

// ConfigStruct returns the struct for reading the configuration file
func (o *EmailOutput) ConfigStruct() interface{} {
	return new(EmailOutputConfig)
}

// Init initializes the givegn EmailOutput instance by
//extracting from, to, sid and token value from config
//and store it on the plugin instance.
func (o *EmailOutput) Init(config interface{}) error {
	conf := config.(*EmailOutputConfig)
	o.hostport = conf.Address
	if o.hostport != "" {
		host := o.hostport
		if i := strings.Index(host, ":"); i >= 0 {
			host = host[:i]
		} else {
			o.hostport = host + ":25"
		}
		if conf.Username != "" {
			o.auth = smtp.PlainAuth("", conf.Username, conf.Password, host)
		}
	}
	o.From, o.To = conf.From, conf.To
	if conf.NoCertCheck {
		o.tlsConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return o.Prepare()
}

//Prepare prepares the sending (gets MX records if no hostport is given)
func (o *EmailOutput) Prepare() error {
	if o.hostport == "" {
		var (
			i    int
			ok   bool
			host string
			err  error
			tos  []string
			mxs  []*net.MX
		)
		o.byHost = make(map[string][]string, len(o.To))
		for _, tos := range o.To {
			i = strings.Index(tos, "@")
			host = tos[i+1:]
			o.byHost[host] = append(o.byHost[host], tos)
		}
		for host, tos = range o.byHost {
			mxAddrsLock.Lock()
			if mxs, ok = mxAddrs[host]; !ok {
				if mxs, err = net.LookupMX(host); err != nil {
					return fmt.Errorf("error looking up MX record for %s: %s", host, err)
				}
				mxAddrs[host] = mxs
			}
			mxAddrsLock.Unlock()
			ok = false
			for _, mx := range mxs {
				log.Printf("test sending with %s to %s", mx.Host, tos)
				err = testMail(mx.Host+":25", nil, o.From, tos, 10*time.Second,
					o.tlsConfig)
				log.Printf("test send with %s to %s result: %s", mx.Host, tos, err)
				if err == nil {
					ok = true
					break
				}
			}
			if !ok {
				return fmt.Errorf("error test sending mail from %s to %s with %s: %s",
					o.From, tos, mxs, err)
			}
		}
		return nil
	}
	o.byHost = make(map[string][]string, 1)
	log.Printf("test sending with %s to %s", o.hostport, o.To)
	err := testMail(o.hostport, o.auth, o.From, o.To, 10*time.Second, o.tlsConfig)
	log.Printf("test send with %s to %s result: %s", o.hostport, o.To, err)
	if err == nil {
		o.byHost[""] = o.To
	}
	return err
}

//type Output interface {
//       Run(or OutputRunner, h PluginHelper) (err error)
//    }
//

// Run is the plugin's main loop
//iterates over received messages, checking against
//message hostname and delivering to the output if hostname is in our config.
func (o *EmailOutput) Run(runner pipeline.OutputRunner, helper pipeline.PluginHelper) (
	err error) {

	var (
		payload string
	)
	body := bytes.NewBuffer(nil)

	for pack := range runner.InChan() {
		payload = pack.Message.GetPayload()
		if len(payload) > 100 {
			payload = payload[:100]
		}
		body.WriteString(fmt.Sprintf("Subject: %s [%d] %s@%s: ",
			utils.TsTime(pack.Message.GetTimestamp()).Format(time.RFC3339),
			pack.Message.GetSeverity(), pack.Message.GetLogger(),
			pack.Message.GetHostname()))
		body.WriteString(payload)
		body.WriteString("\r\n\r\n")
		body.WriteString(pack.Message.GetPayload())
		pack.Recycle()
		err = o.sendMail(body.Bytes())
		body.Reset()
		if err != nil {
			return fmt.Errorf("error sending email: %s", err)
		}

	}
	return
}

var mxAddrs = make(map[string][]*net.MX, 16)
var mxAddrsLock = sync.Mutex{}

// sendMail sends mail using smtp.SendMail but looks up MX records if no hostport is provided
func (o EmailOutput) sendMail(body []byte) error {
	if o.hostport == "" {
		var (
			host string
			err  error
			tos  []string
			mxs  []*net.MX
		)
		for host, tos = range o.byHost {
			mxAddrsLock.Lock()
			mxs = mxAddrs[host]
			mxAddrsLock.Unlock()
			err = nil
			for _, mx := range mxs {
				log.Printf("sending with %s to %s", mx.Host, tos)
				err = sendMail(mx.Host+":25", nil, o.From, tos, body,
					30, o.tlsConfig)
				log.Printf("send with %s to %s result: %s", mx.Host, tos, err)
				if err == nil {
					break
				}
			}
			if err != nil {
				return fmt.Errorf("error sending mail from %s to %s with %s: %s",
					o.From, tos, mxs, err)
			}
		}
		return nil
	}
	log.Printf("sending with %s to %s", o.hostport, o.To)
	err := sendMail(o.hostport, o.auth, o.From, o.To, body,
		DefaultTimeout, o.tlsConfig)
	log.Printf("send with %s to %s result: %s", o.hostport, o.To, err)
	return err
}

// testMail connects to the server at addr, switches to TLS if possible,
// authenticates with mechanism a if possible, and then tests sending an email from
// address from, to addresses to
func testMail(addr string, a smtp.Auth, from string, to []string, timeout time.Duration, tlsConfig *tls.Config) error {
	return sendMail(addr, a, from, to, nil, timeout, tlsConfig)
}

// sendMail connects to the server at addr, switches to TLS if possible (using the given config),
// authenticates with mechanism a if possible, and then sends an email from
// address from, to addresses to, with message msg.
//
// If msg is nil, then quits, this testing the recipients and the server
func sendMail(addr string, a smtp.Auth, from string, to []string, msg []byte, timeout time.Duration, tlsConfig *tls.Config) error {
	//c, err := Dial(addr)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	//if err := c.hello(); err != nil {
	//    return err
	//}
	if err := c.Hello("localhost"); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err = c.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if a != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err = c.Auth(a); err != nil {
				return err
			}
		}
	}
	if err = c.Mail(from); err != nil {
		return err
	}
	for _, addr := range to {
		if err = c.Rcpt(addr); err != nil {
			return err
		}
	}
	if msg != nil {
		w, err := c.Data()
		if err != nil {
			return err
		}
		_, err = w.Write(msg)
		if err != nil {
			return err
		}
		err = w.Close()
		if err != nil {
			return err
		}
	}
	return c.Quit()
}

func init() {
	pipeline.RegisterPlugin("EmailOutput", func() interface{} { return new(EmailOutput) })
}
