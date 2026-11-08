// Package version matches version numbers against constraints, the way a model would write
// them for a language or a package: the npm style ("^3.12", "~3.13", "3.14.x", ">=3.10 <3.13",
// "3.12 || 3.13") and the Python style ("==3.12.*", ">=3.10,<3.13", "~=3.12"). Versions are
// dotted numbers. There are no pre-releases, since nothing tack offers has them.
package version

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is a dotted version number, like 3.14.1.
type Version []int

// Parse reads a version of one to four numbers.
func Parse(text string) (Version, error) {
	parts := strings.Split(text, ".")
	if len(parts) > 4 {
		return nil, fmt.Errorf("%q has too many parts", text)
	}
	out := make(Version, len(parts))
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || part == "" || part[0] == '+' || len(part) > 1 && part[0] == '0' {
			return nil, fmt.Errorf("%q isn't a version", text)
		}
		out[i] = n
	}
	return out, nil
}

func (v Version) String() string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// compare compares two versions, with missing parts counting as zero.
func compare(a, b Version) int {
	for i := range max(len(a), len(b)) {
		x, y := at(a, i), at(b, i)
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func at(v Version, i int) int {
	if i < len(v) {
		return v[i]
	}
	return 0
}

// bump is the first version after every one that starts with v's first n parts.
func bump(v Version, n int) Version {
	out := make(Version, n)
	copy(out, v)
	out[n-1]++
	return out
}

// bound is one comparison, like >=3.12.
type bound struct {
	op string
	v  Version
}

func (b bound) holds(v Version) bool {
	c := compare(v, b.v)
	switch b.op {
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case "!=":
		return c != 0
	}
	return c == 0
}

// Constraint is any of several sets of bounds, each of which must all hold.
type Constraint struct {
	text string
	any  [][]bound
}

func (c Constraint) String() string { return c.text }

// ParseConstraint reads a constraint. Clauses joined by commas or spaces must all hold, and
// "||" separates alternatives. Empty, "*", "x" and "latest" mean any version.
func ParseConstraint(text string) (Constraint, error) {
	out := Constraint{text: text}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || trimmed == "*" || trimmed == "x" || trimmed == "latest" {
		out.any = [][]bound{nil}
		return out, nil
	}
	for _, alternative := range strings.Split(trimmed, "||") {
		var all []bound
		fields := strings.FieldsFunc(alternative, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		if len(fields) == 0 {
			return Constraint{}, fmt.Errorf("%q has an empty alternative", text)
		}
		for i := 0; i < len(fields); i++ {
			field := fields[i]
			// "3.1 - 3.4" is a range.
			if i+2 < len(fields) && fields[i+1] == "-" {
				low, err := clause(">=" + field)
				if err != nil {
					return Constraint{}, err
				}
				high, err := clause("<=" + fields[i+2])
				if err != nil {
					return Constraint{}, err
				}
				all = append(all, low...)
				all = append(all, high...)
				i += 2
				continue
			}
			// ">= 3.12" with a space.
			if strings.Trim(field, "<>=!~^") == "" && i+1 < len(fields) {
				field += fields[i+1]
				i++
			}
			bounds, err := clause(field)
			if err != nil {
				return Constraint{}, err
			}
			all = append(all, bounds...)
		}
		out.any = append(out.any, all)
	}
	return out, nil
}

// clause reads one comparison into the bounds it means.
func clause(text string) ([]bound, error) {
	op := ""
	for _, candidate := range []string{"===", "==", ">=", "<=", "!=", "~=", ">", "<", "=", "~", "^"} {
		if strings.HasPrefix(text, candidate) {
			op = candidate
			break
		}
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(text, op), "v")
	// Wildcards: 3.12.*, 3.12.x and 3.x mean every version starting with what's before them.
	wild := false
	for _, suffix := range []string{".*", ".x", ".X"} {
		if strings.HasSuffix(rest, suffix) {
			rest, wild = strings.TrimSuffix(rest, suffix), true
		}
	}
	if rest == "*" || rest == "x" || rest == "X" {
		if op == "" || op == "=" || op == "==" {
			return nil, nil
		}
		return nil, fmt.Errorf("%q compares with any version", text)
	}
	v, err := Parse(rest)
	if err != nil {
		return nil, err
	}
	n := len(v)
	switch op {
	case "", "=", "==", "===":
		// A version with fewer parts means every version that starts with it, as 3.12 means 3.12.*.
		if wild || op == "" || op == "=" || op == "==" {
			return []bound{{">=", v}, {"<", bump(v, n)}}, nil
		}
		return []bound{{"==", v}}, nil
	case "!=":
		if wild {
			return nil, errors.New("a wildcard can't follow !=")
		}
		return []bound{{"!=", v}}, nil
	case "~":
		// ~3.13 and ~3.13.2 allow changes after the second part. ~3 allows changes after the first.
		keep := min(n, 2)
		return []bound{{">=", v}, {"<", bump(v, keep)}}, nil
	case "^":
		// ^3.12 allows anything until the first part that isn't zero changes.
		keep := 1
		for keep < n && v[keep-1] == 0 {
			keep++
		}
		return []bound{{">=", v}, {"<", bump(v, keep)}}, nil
	case "~=":
		// Python's compatible release: ~=3.12 means >=3.12,<4, and ~=3.12.1 means >=3.12.1,<3.13.
		if n < 2 {
			return nil, fmt.Errorf("%q needs at least two parts after ~=", text)
		}
		return []bound{{">=", v}, {"<", bump(v, n-1)}}, nil
	}
	if wild {
		return nil, fmt.Errorf("a wildcard can't follow %s", op)
	}
	return []bound{{op, v}}, nil
}

// Allows says whether v meets the constraint.
func (c Constraint) Allows(v Version) bool {
	for _, all := range c.any {
		ok := true
		for _, b := range all {
			if !b.holds(v) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
