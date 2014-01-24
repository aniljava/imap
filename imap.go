package imap

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
)

const (
	RFC822Header = "rfc822.header"
	RFC822Text   = "rfc822.text"
	Seen         = "\\Seen"
	Deleted      = "\\Deleted"
	Inbox        = "INBOX"
)

type IMAPClient struct {
	conn  *tls.Conn
	count int
	buf   []byte
}

func NewClient(conn net.Conn, hostname string) (*IMAPClient, error) {
	config := tls.Config{
		ServerName: hostname,
	}
	c := tls.Client(conn, &config)
	buf := make([]byte, 1024)
REPLY:
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		for _, i := range buf[:n] {
			if i == byte('\n') {
				break REPLY
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return &IMAPClient{
		conn: c,
		buf:  buf,
	}, nil
}

func (c *IMAPClient) Close() error {
	return c.conn.Close()
}

func (c *IMAPClient) Do(cmd string) *Response {
	c.count++
	cmd = fmt.Sprintf("a%03d %s\r\n", c.count, cmd)
	ret := NewResponse()

	_, err := c.conn.Write([]byte(cmd))
	if err != nil {
		ret.err = err
		return ret
	}

	for {
		n, err := c.conn.Read(c.buf)
		if err != nil {
			ret.err = err
			return ret
		}
		isFinished, err := ret.Feed(c.buf[:n])
		if err != nil {
			ret.err = err
			return ret
		}
		if isFinished {
			break
		}
	}
	return ret
}

func (c *IMAPClient) Login(user, password string) error {
	resp := c.Do(fmt.Sprintf("LOGIN %s %s", user, password))
	return resp.err
}

func (c *IMAPClient) Select(box string) *Response {
	return c.Do(fmt.Sprintf("SELECT %s", box))
}

func (c *IMAPClient) Search(flag string) ([]string, error) {
	resp := c.Do(fmt.Sprintf("SEARCH %s", flag))
	if resp.Error() != nil {
		return nil, resp.Error()
	}
	for _, reply := range resp.Replys() {
		org := reply.Origin()
		if len(org) >= 6 && strings.ToUpper(org[:6]) == "SEARCH" {
			ids := strings.Trim(org[6:], " \t\n\r")
			if ids == "" {
				return nil, nil
			}
			return strings.Split(ids, " "), nil
		}
	}
	return nil, errors.New("Invalid response")
}

func (c *IMAPClient) Fetch(id, arg string) (string, error) {
	resp := c.Do(fmt.Sprintf("FETCH %s %s", id, arg))
	if resp.Error() != nil {
		return "", resp.Error()
	}
	for _, reply := range resp.Replys() {
		org := reply.Origin()
		if len(org) < len(id) || org[:len(id)] != id {
			continue
		}
		org = org[len(id)+1:]
		if len(org) >= 5 && strings.ToUpper(org[:5]) == "FETCH" {
			body := reply.Content()
			i := strings.Index(body, "\n")
			return body[i+1:], nil
		}
	}
	return "", errors.New("Invalid response")
}

func (c *IMAPClient) StoreFlag(id, flag string) error {
	resp := c.Do(fmt.Sprintf("STORE %s FLAGS %s", id, flag))
	return resp.Error()
}

func (c *IMAPClient) Logout() error {
	resp := c.Do("LOGOUT")
	return resp.Error()
}

func (c *IMAPClient) GetMessage(id string) (*mail.Message, error) {
	headerResp := c.Do(fmt.Sprintf("FETCH %s %s", id, RFC822Header))
	if headerResp.Error() != nil {
		return nil, headerResp.Error()
	}

	replys := headerResp.Replys()

	reader := textproto.NewReader(bufio.NewReader(bytes.NewBuffer(replys[0].content)))
	header, err := reader.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}

	bodyResp := c.Do(fmt.Sprintf("FETCH %s %s", id, RFC822Text))
	if bodyResp.Error() != nil {
		return nil, bodyResp.Error()
	}

	return &mail.Message{
		Header: mail.Header(header),
		Body:   bytes.NewBuffer(bodyResp.Replys()[0].content),
	}, nil
}

type reply struct {
	origin  []byte
	type_   []byte
	length  []byte
	content []byte
}

func newReply() (ret reply) {
	return reply{
		origin:  make([]byte, 0, 0),
		type_:   make([]byte, 0, 0),
		content: make([]byte, 0, 0),
	}
}

func (r reply) Origin() string {
	return string(r.origin)
}

func (r reply) Type() string {
	return string(r.type_)
}

func (r reply) Length() (i int, err error) {
	i, err = strconv.Atoi(string(r.length))
	return
}

func (r reply) Content() string {
	return string(r.content)
}

type feedStatus int

const (
	feedInit feedStatus = iota
	feedStar
	feedReply
	feedReplyType
	feedReplyLength
	feedReplyContent
	feedReplyMeet0d
	feedStatusLine
	feedStatusLineMeet0d
	feedFinished
)

type Response struct {
	id     string
	status string
	err    error
	replys []reply

	buf              []byte
	feedStatus       feedStatus
	parenthesisCount int
	reply            reply
}

func NewResponse() *Response {
	return &Response{
		buf:              make([]byte, 0, 0),
		feedStatus:       feedInit,
		parenthesisCount: 0,
	}
}

func (r *Response) Feed(input []byte) (bool, error) {
	for _, i := range input {
		switch r.feedStatus {
		case feedInit:
			if i == byte('*') {
				r.feedStatus = feedStar
			} else {
				r.feedStatus = feedStatusLine
				r.buf = append(r.buf, i)
			}
		case feedStar:
			if i != byte(' ') {
				r.feedStatus = feedReply
				r.reply = newReply()
				r.reply.origin = append(r.reply.origin, i)
			}
		case feedReply:
			switch i {
			case byte('\r'):
				r.feedStatus = feedReplyMeet0d
			case byte('('):
				r.feedStatus = feedReplyType
				r.reply.origin = append(r.reply.origin, i)
			default:
				r.reply.origin = append(r.reply.origin, i)
			}
		case feedReplyType:
			switch i {
			case byte(')'):
				r.feedStatus = feedReply
			case byte(' '):
				if len(r.reply.type_) > 0 {
					r.feedStatus = feedReplyLength
				}
			default:
				r.reply.type_ = append(r.reply.type_, i)
			}
			r.reply.origin = append(r.reply.origin, i)
		case feedReplyLength:
			r.reply.origin = append(r.reply.origin, i)
			if i == byte('\n') {
				r.feedStatus = feedReplyContent
			}
			if byte('0') <= i && i <= byte('9') {
				r.reply.length = append(r.reply.length, i)
			}
		case feedReplyContent:
			r.reply.origin = append(r.reply.origin, i)
			r.reply.content = append(r.reply.content, i)
			i, err := r.reply.Length()
			if err != nil {
				return false, errors.New("Parse response error, reply need a valid length number")
			}
			if len(r.reply.content) == i {
				r.feedStatus = feedReply
			}
		case feedReplyMeet0d:
			if i == byte('\n') {
				r.feedStatus = feedInit
				r.replys = append(r.replys, r.reply)
				r.buf = r.buf[0:0]
			} else {
				r.feedStatus = feedReply
				r.reply.origin = append(r.reply.origin, i)
			}
		case feedStatusLine:
			if i == byte('\r') {
				r.feedStatus = feedStatusLineMeet0d
			} else {
				r.buf = append(r.buf, i)
			}
		case feedStatusLineMeet0d:
			if i == byte('\n') {
				r.feedStatus = feedFinished
				array := strings.SplitN(string(r.buf), " ", 2)
				if len(array) > 0 {
					r.id = array[0]
				}
				if len(array) > 1 {
					r.status = array[1]
				}
				if len(r.status) < 3 || r.status[:3] != "OK " {
					r.err = errors.New(r.status)
				}
				return true, nil
			} else {
				r.feedStatus = feedStatusLine
				r.buf = append(r.buf, byte('\r'), i)
			}
		case feedFinished:
			return true, errors.New("Need no more feed")
		}
	}
	return false, nil
}

func (r *Response) Id() string {
	return r.id
}

func (r *Response) Status() string {
	return r.status
}

func (r *Response) Error() error {
	return r.err
}

func (r *Response) Replys() []reply {
	return r.replys
}
