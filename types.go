package mailmantasks

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const Version = "0.1.0"
const (
	TargetFrom            = "from"
	TargetSubject         = "subject"
	TargetBody            = "body"
	ComparatorIncludes    = "includes"
	ComparatorExcludes    = "excludes"
	ComparatorMatches     = "matches"
	ComparatorFindByRegex = "findByRegex"
	FormatterText         = "text"
	FormatterHTML         = "html"
	FormatterLink         = "link"
	FormatterNumeric      = "numeric"
	RegexSourcePretty     = "pretty"
	RegexSourceRaw        = "raw"
	StatusPending         = "pending"
	StatusSuccess         = "success"
	StatusError           = "error"
	StatusTimeout         = "timeout"
)

// MailTask uses seconds for ReceivedAt (Unix time) and Timeout.
// Zero values select client defaults; timeouts above 3600 are clamped.
type MailTask struct {
	Email        string        `json:"email"`
	ReceivedAt   int64         `json:"receivedAt"`
	Timeout      uint32        `json:"timeout"`
	Group        string        `json:"group"`
	TemplateID   string        `json:"templateId,omitempty"`
	MailFilters  []MailFilter  `json:"mailFilters"`
	MailElements []MailElement `json:"mailElements"`
}
type MailFilter struct {
	Target          string   `json:"target"`
	Comparator      string   `json:"comparator"`
	Value           string   `json:"value"`
	OrValues        []string `json:"orValues,omitempty"`
	Optional        bool     `json:"optional,omitempty"`
	RegexReplaceAll string   `json:"regexReplaceAll,omitempty"`
}
type MailElement struct {
	Name        string `json:"name"`
	Target      string `json:"target"`
	Selector    string `json:"selector,omitempty"`
	Regex       string `json:"regex,omitempty"`
	RegexSource string `json:"regexSource,omitempty"`
	Formatter   string `json:"formatter,omitempty"`
	Penetrate   bool   `json:"penetrate,omitempty"`
}
type TaskResult struct {
	ID      string            `json:"id"`
	Status  string            `json:"status"`
	Results map[string]string `json:"results,omitempty"`
}

func invalid(message string) error {
	return &InvalidRequestError{&MailmanError{Status: 400, Message: message}}
}
func checkLen(field, value string) error {
	if utf8.RuneCountInString(value) > 1024 {
		return invalid(field + " exceeds 1024 characters")
	}
	return nil
}
func enum(field, value string, choices ...string) error {
	for _, c := range choices {
		if value == c {
			return nil
		}
	}
	return invalid(fmt.Sprintf("%s has invalid value %q", field, value))
}
func validate(t MailTask) error {
	for _, f := range []struct{ k, v string }{{"email", t.Email}, {"group", t.Group}, {"templateId", t.TemplateID}} {
		if err := checkLen(f.k, f.v); err != nil {
			return err
		}
	}
	if strings.TrimSpace(t.Email) == "" {
		return invalid("email is required")
	}
	if t.ReceivedAt <= 0 {
		return invalid("receivedAt is required")
	}
	if len(t.MailFilters) == 0 {
		return invalid("mailFilters is required")
	}
	if len(t.MailFilters) > 10 {
		return invalid("mailFilters has too many entries")
	}
	if len(t.MailElements) == 0 {
		return invalid("mailElements is required")
	}
	if len(t.MailElements) > 10 {
		return invalid("mailElements has too many entries")
	}
	for i, f := range t.MailFilters {
		at := fmt.Sprintf("mailFilters[%d].", i)
		for _, v := range []struct{ k, v string }{{"target", f.Target}, {"comparator", f.Comparator}, {"value", f.Value}, {"regexReplaceAll", f.RegexReplaceAll}} {
			if err := checkLen(at+v.k, v.v); err != nil {
				return err
			}
		}
		if len(f.OrValues) > 10 {
			return invalid("orValues has too many entries")
		}
		for _, v := range f.OrValues {
			if err := checkLen(at+"orValues", v); err != nil {
				return err
			}
		}
		if err := enum(at+"target", f.Target, TargetFrom, TargetSubject); err != nil {
			return err
		}
		if err := enum(at+"comparator", f.Comparator, ComparatorIncludes, ComparatorExcludes, ComparatorMatches, ComparatorFindByRegex); err != nil {
			return err
		}
		if f.Value == "" {
			return invalid("mailFilters[].value is required")
		}
	}
	for i, e := range t.MailElements {
		at := fmt.Sprintf("mailElements[%d].", i)
		for _, v := range []struct{ k, v string }{{"name", e.Name}, {"target", e.Target}, {"selector", e.Selector}, {"regex", e.Regex}, {"regexSource", e.RegexSource}, {"formatter", e.Formatter}} {
			if err := checkLen(at+v.k, v.v); err != nil {
				return err
			}
		}
		if e.Name == "" {
			return invalid("mailElements[].name is required")
		}
		if err := enum(at+"target", e.Target, TargetFrom, TargetSubject, TargetBody); err != nil {
			return err
		}
		if e.Target == TargetBody && e.Selector == "" && e.Regex == "" {
			return invalid("mailElements[].selector or regex is required")
		}
		if err := enum(at+"regexSource", e.RegexSource, "", RegexSourcePretty, RegexSourceRaw); err != nil {
			return err
		}
		if err := enum(at+"formatter", e.Formatter, "", FormatterText, FormatterHTML, FormatterLink, FormatterNumeric); err != nil {
			return err
		}
	}
	return nil
}

// CodeTask extracts a digit code into results["code"].
func CodeTask(email, from, subjectIncludes string, length int) MailTask {
	t := MailTask{Email: email, MailFilters: []MailFilter{
		{Target: TargetFrom, Comparator: ComparatorIncludes, Value: from},
		{Target: TargetSubject, Comparator: ComparatorIncludes, Value: subjectIncludes},
	}}
	t.MailElements = []MailElement{{Name: "code", Target: TargetBody, Regex: fmt.Sprintf(`\b\d{%d}\b`, length)}}
	return t
}

// LinkTask extracts the first /login link into results["loginUrl"].
func LinkTask(email, from, subjectIncludes string) MailTask {
	t := CodeTask(email, from, subjectIncludes, 6)
	t.MailElements = []MailElement{{Name: "loginUrl", Target: TargetBody, Selector: "a[href*='/login']", Formatter: FormatterLink}}
	return t
}
