// Package tmpl contains types to compile and interpolate name templates
// with a given context.
package tmpl

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"

	"github.com/mesosphere/mesos-dns/logging"
	"github.com/mesosphere/mesos-dns/records/labels"
	"github.com/mesosphere/mesos-dns/records/state"
)

type (
	// Context is the namespace to resolve Name template variables in.
	Context map[string]string

	// Template holds a text/template style tempalte for a DNS name.
	Template string

	// Compiled is a compiled Template that can be executed efficiently
	Compiled struct {
		Template
		tokens []token
	}

	token interface {
		interpolate(Context) (string, error)
		isSeparator() bool
	}

	separatorToken struct{}
	stringToken    string
	variableToken  string
)

// DefaultTemplates returns a the default name templates.
func DefaultTemplates() []Template { return []Template{"{name}.{framework}"} }

// addNonVariableTokens splits the given string s into partial labels and adds
// tokens for them. It accepts labels with "_" depending on whether s is in the
// left or right most position in the pattern.
func addNonVariableTokens(tokens []token, s string, leftMost, rightMost bool, spec labels.Func) ([]token, error) {
	if s == "" {
		return tokens, nil
	}

	if s == "." {
		tokens = append(tokens, separatorToken{})
		return tokens, nil
	}

	labels := strings.Split(s, ".")
	for i, label := range labels {
		if i != 0 {
			tokens = append(tokens, separatorToken{})
		}

		// "" and first or last => . at the left or right of s, skip empty string
		if label == "" && (i == 0 || i == len(labels)-1) {
			continue
		}

		// "" and not the first or last => consecutive separators
		if label == "" && i > 0 && i < len(labels)-1 {
			return nil, fmt.Errorf("invalid consecutive separators")
		}

		// special case for valid underscores: trim them for spec comparison below
		labelWithoutValidUnderscore := label
		if (label[0] == '_' && label != "_") || (label == "_" && !rightMost) {
			labelWithoutValidUnderscore = strings.TrimLeft(label, "_")
		}

		// prepend or append some character to check for RFC compatibility for non-inner labels.
		//
		// But don't do this if there is no variableToken on the left or the right, i.e. these
		// tokens are the most left or the most right ones in the template.
		pre := ""
		post := ""
		if i == 0 && !leftMost {
			pre = "a"
		}
		if i == len(labels)-1 && !rightMost {
			post = "a"
		}

		if escapedLabel := spec(pre + labelWithoutValidUnderscore + post); pre+labelWithoutValidUnderscore+post != escapedLabel {
			return nil, fmt.Errorf("templte substring %v is no valid label", label)
		}
		tokens = append(tokens, stringToken(label))
	}
	return tokens, nil
}

// Compile compiles a Template to a fast Compiled template.
func (t Template) Compile(spec labels.Func) (*Compiled, error) {
	tokens := []token{}

	if string(t) == "" {
		return nil, fmt.Errorf("invalid empty template")
	}

	// split template into tokens: strings, separators and {variables}
	varRE, err := regexp.Compile(`{[\s\w-:]*}`)
	if err != nil {
		logging.Error.Fatalf("invalid regular expression for variables in template: %v", err)
	}

	// find variable references and work through the index list
	varMatches := varRE.FindAllStringIndex(string(t), -1)
	oldRight := 0
	for i, m := range varMatches {
		// extract variable identifier
		left := m[0]
		right := m[1]
		identifier := strings.Trim(string(t)[left+1:right-1], " \t")
		if identifier == "" {
			return nil, fmt.Errorf("empty variable reference found in template %v", t)
		}

		// create token for everything in front of the variable
		leftMost := i == 0
		tokens, err = addNonVariableTokens(tokens, string(t)[oldRight:left], leftMost, false, spec)
		if err != nil {
			return nil, fmt.Errorf("invalid template %v: %v", t, err)
		}

		// add the actual variable token
		tokens = append(tokens, variableToken(identifier))

		// prepare for next round
		oldRight = right
	}

	// add pending tokens behind the last variable
	leftMost := len(varMatches) == 0
	rightMost := true
	tokens, err = addNonVariableTokens(tokens, string(t)[oldRight:len(t)], leftMost, rightMost, spec)
	if err != nil {
		return nil, fmt.Errorf("invalid template %v: %v", t, err)
	}

	// check that the first and the last token is not a separator
	if tokens[0].isSeparator() {
		return nil, fmt.Errorf("template cannot start with a dot")
	}
	if tokens[len(tokens)-1].isSeparator() {
		return nil, fmt.Errorf("template cannot end with a dot")
	}

	return &Compiled{
		Template: t,
		tokens:   tokens,
	}, nil
}

// String returns the template string
func (t Template) String() string { return string(t) }

// Execute applies a Context to a pre-compiled Template by interpolating
// the context values using the text.Template syntax.
func (c *Compiled) Execute(ctx Context, domain string) (string, error) {
	labels := make([]string, 0, len(c.tokens)+3)
	for _, t := range c.tokens {
		label, err := t.interpolate(ctx)
		if err != nil {
			return "", err
		}
		labels = append(labels, label)
	}
	labels = append(labels, ".", domain, ".")
	return strings.Join(labels, ""), nil
}

func (separatorToken) interpolate(Context) (string, error) { return ".", nil }
func (t stringToken) interpolate(Context) (string, error)  { return string(t), nil }
func (t variableToken) interpolate(ctx Context) (string, error) {
	value := ctx[string(t)]
	if value == "" {
		return "", fmt.Errorf("%q is not defined in context %v", t, ctx)
	}
	return value, nil
}

func (separatorToken) isSeparator() bool { return true }
func (stringToken) isSeparator() bool    { return false }
func (variableToken) isSeparator() bool  { return false }

// NewContext creates a template Context for a given task and a label spec.
func NewContext(task *state.Task, framework string, spec labels.Func) Context {
	context := Context{
		"framework":      framework,
		"slave-id-short": slaveIDTail(task.SlaveID),
		"slave-id":       task.SlaveID,
		"task-id":        task.ID,
		"task-id-hash":   hashString(task.ID),
		"name":           specEachLabel(task.Name, spec),
	}

	if task.Discovery != nil {
		possiblySet := func(key string, value *string) {
			if value != nil && *value != "" {
				context[key] = specEachLabel(*value, spec)
			}
		}
		possiblySet("version", task.Discovery.Version)
		possiblySet("location", task.Discovery.Location)
		possiblySet("environment", task.Discovery.Environment)

		for _, label := range task.Discovery.Labels.Labels {
			context["label:"+label.Key] = spec(label.Value)
		}

		// use discovery name of task name if defined
		if task.Discovery.Name != nil && *task.Discovery.Name != "" {
			context["name"] = spec(*task.Discovery.Name)
		}
	}
	return context
}

func specEachLabel(s string, spec labels.Func) string {
	labels := strings.Split(s, ".")
	specLabels := make([]string, 0, len(labels))
	for _, l := range labels {
		specLabels = append(specLabels, spec(l))
	}
	return strings.Join(specLabels, ".")
}

// return the slave number from a Mesos slave id
func slaveIDTail(slaveID string) string {
	fields := strings.Split(slaveID, "-")
	return strings.ToLower(fields[len(fields)-1])
}

// BUG: The probability of hashing collisions is too high with only 17 bits.
// NOTE: Using a numerical base as high as valid characters in DNS names would
// reduce the resulting length without risking more collisions.
func hashString(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	sum := h.Sum32()
	lower, upper := uint16(sum), uint16(sum>>16)
	return strconv.FormatUint(uint64(lower+upper), 10)
}
