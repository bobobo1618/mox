package imapserver

import (
	"errors"
	"fmt"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/mjl-/mox/mlog"
)

var (
	listWildcards  = "%*"
	char           = charRange('\x01', '\x7f')
	ctl            = charRange('\x01', '\x19')
	quotedSpecials = `"\`
	respSpecials   = "]"
	atomChar       = charRemove(char, "(){ "+ctl+listWildcards+quotedSpecials+respSpecials)
	astringChar    = atomChar + respSpecials
)

func charRange(first, last rune) string {
	r := ""
	c := first
	r += string(c)
	for c < last {
		c++
		r += string(c)
	}
	return r
}

func charRemove(s, remove string) string {
	r := ""
next:
	for _, c := range s {
		for _, x := range remove {
			if c == x {
				continue next
			}
		}
		r += string(c)
	}
	return r
}

type parser struct {
	// Orig is the line in original casing, and upper in upper casing. We often match
	// against upper for easy case insensitive handling as IMAP requires, but sometimes
	// return from orig to keep the original case.
	orig     string
	upper    string
	o        int      // Current offset in parsing.
	contexts []string // What we're parsing, for error messages.
	conn     *conn
}

// toUpper upper cases bytes that are a-z. strings.ToUpper does too much. and
// would replace invalid bytes with unicode replacement characters, which would
// break our requirement that offsets into the original and upper case strings
// point to the same character.
func toUpper(s string) string {
	r := []byte(s)
	for i, c := range r {
		if c >= 'a' && c <= 'z' {
			r[i] = c - 0x20
		}
	}
	return string(r)
}

func newParser(s string, conn *conn) *parser {
	return &parser{s, toUpper(s), 0, nil, conn}
}

func (p *parser) xerrorf(format string, args ...any) {
	var err error
	errmsg := fmt.Sprintf(format, args...)
	remaining := fmt.Sprintf("remaining %q", p.orig[p.o:])
	if len(p.contexts) > 0 {
		remaining += ", context " + strings.Join(p.contexts, ",")
	}
	remaining = " (" + remaining + ")"
	if p.conn.account != nil {
		errmsg += remaining
		err = errors.New(errmsg)
	} else {
		err = errors.New(errmsg + remaining)
	}
	panic(syntaxError{"", "", errmsg, err})
}

func (p *parser) context(s string) func() {
	p.contexts = append(p.contexts, s)
	return func() {
		p.contexts = p.contexts[:len(p.contexts)-1]
	}
}

func (p *parser) empty() bool {
	return p.o == len(p.upper)
}

func (p *parser) xempty() {
	if !p.empty() {
		p.xerrorf("leftover data")
	}
}

func (p *parser) hasPrefix(s string) bool {
	return strings.HasPrefix(p.upper[p.o:], s)
}

func (p *parser) take(s string) bool {
	if !p.hasPrefix(s) {
		return false
	}
	p.o += len(s)
	return true
}

func (p *parser) xtake(s string) {
	if !p.take(s) {
		p.xerrorf("expected %s", s)
	}
}

func (p *parser) xnonempty() {
	if p.empty() {
		p.xerrorf("unexpected end")
	}
}

func (p *parser) xtakeall() string {
	r := p.orig[p.o:]
	p.o = len(p.orig)
	return r
}

func (p *parser) xtake1n(n int, what string) string {
	if n == 0 {
		p.xerrorf("expected chars from %s", what)
	}
	return p.xtaken(n)
}

func (p *parser) xtake1fn(fn func(i int, c rune) bool) string {
	i := 0
	s := ""
	for _, c := range p.upper[p.o:] {
		if !fn(i, c) {
			break
		}
		s += string(c)
		i++
	}
	if s == "" {
		p.xerrorf("expected at least one character")
	}
	p.o += len(s)
	return s
}

func (p *parser) xtakechars(s string, what string) string {
	p.xnonempty()
	for i, c := range p.orig[p.o:] {
		if !contains(s, c) {
			return p.xtake1n(i, what)
		}
	}
	return p.xtakeall()
}

func (p *parser) xtaken(n int) string {
	if p.o+n > len(p.orig) {
		p.xerrorf("not enough data")
	}
	r := p.orig[p.o : p.o+n]
	p.o += n
	return r
}

func (p *parser) peekn(n int) (string, bool) {
	if len(p.upper[p.o:]) < n {
		return "", false
	}
	return p.upper[p.o : p.o+n], true
}

func (p *parser) space() bool {
	return p.take(" ")
}

func (p *parser) xspace() {
	if !p.space() {
		p.xerrorf("expected space")
	}
}

func (p *parser) digits() string {
	var n int
	for _, c := range p.upper[p.o:] {
		if c < '0' || c > '9' {
			break
		}
		n++
	}
	if n == 0 {
		return ""
	}
	s := p.upper[p.o : p.o+n]
	p.o += n
	return s
}

func (p *parser) nznumber() (uint32, bool) {
	o := p.o
	for o < len(p.upper) && p.upper[o] >= '0' && p.upper[o] <= '9' {
		o++
	}
	if o == p.o {
		return 0, false
	}
	if n, err := strconv.ParseUint(p.upper[p.o:o], 10, 32); err != nil {
		return 0, false
	} else if n == 0 {
		return 0, false
	} else {
		p.o = o
		return uint32(n), true
	}
}

func (p *parser) xnznumber() uint32 {
	n, ok := p.nznumber()
	if !ok {
		p.xerrorf("expected non-zero number")
	}
	return n
}

func (p *parser) number() (uint32, bool) {
	o := p.o
	for o < len(p.upper) && p.upper[o] >= '0' && p.upper[o] <= '9' {
		o++
	}
	if o == p.o {
		return 0, false
	}
	n, err := strconv.ParseUint(p.upper[p.o:o], 10, 32)
	if err != nil {
		return 0, false
	}
	p.o = o
	return uint32(n), true
}

func (p *parser) xnumber() uint32 {
	n, ok := p.number()
	if !ok {
		p.xerrorf("expected number")
	}
	return n
}

func (p *parser) xnumber64() int64 {
	s := p.digits()
	if s == "" {
		p.xerrorf("expected number64")
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		p.xerrorf("parsing number64 %q: %v", s, err)
	}
	return v
}

// l should be a list of uppercase words, the first match is returned
func (p *parser) takelist(l ...string) (string, bool) {
	for _, w := range l {
		if p.take(w) {
			return w, true
		}
	}
	return "", false
}

func (p *parser) xtakelist(l ...string) string {
	w, ok := p.takelist(l...)
	if !ok {
		p.xerrorf("expected one of %s", strings.Join(l, ","))
	}
	return w
}

func (p *parser) xstring() (r string) {
	if p.take(`"`) {
		esc := false
		r := ""
		for i, c := range p.orig[p.o:] {
			if c == '\\' {
				esc = true
			} else if c == '\x00' || c == '\r' || c == '\n' {
				p.xerrorf("invalid nul, cr or lf in string")
			} else if esc {
				if c == '\\' || c == '"' {
					r += string(c)
					esc = false
				} else {
					p.xerrorf("invalid escape char %c", c)
				}
			} else if c == '"' {
				p.o += i + 1
				return r
			} else {
				r += string(c)
			}
		}
		p.xerrorf("missing closing dquote in string")
	}
	size, sync := p.xliteralSize(100*1024, false)
	s := p.conn.xreadliteral(size, sync)
	line := p.conn.readline(false)
	p.orig, p.upper, p.o = line, toUpper(line), 0
	return s
}

func (p *parser) xnil() {
	p.xtake("NIL")
}

// Returns NIL as empty string.
func (p *parser) xnilString() string {
	if p.take("NIL") {
		return ""
	}
	return p.xstring()
}

func (p *parser) xastring() string {
	if p.hasPrefix(`"`) || p.hasPrefix("{") || p.hasPrefix("~{") {
		return p.xstring()
	}
	return p.xtakechars(astringChar, "astring")
}

func contains(s string, c rune) bool {
	for _, x := range s {
		if x == c {
			return true
		}
	}
	return false
}

func (p *parser) xtag() string {
	p.xnonempty()
	for i, c := range p.orig[p.o:] {
		if c == '+' || !contains(astringChar, c) {
			return p.xtake1n(i, "tag")
		}
	}
	return p.xtakeall()
}

func (p *parser) xcommand() string {
	for i, c := range p.upper[p.o:] {
		if !(c >= 'A' && c <= 'Z' || c == ' ' && p.upper[p.o:p.o+i] == "UID") {
			return p.xtake1n(i, "command")
		}
	}
	return p.xtakeall()
}

func (p *parser) remainder() string {
	return p.orig[p.o:]
}

// ../rfc/9051:6565
func (p *parser) xflag() string {
	w, _ := p.takelist(`\`, "$")
	s := w + p.xatom()
	if s[0] == '\\' {
		switch strings.ToLower(s) {
		case `\answered`, `\flagged`, `\deleted`, `\seen`, `\draft`:
		default:
			p.xerrorf("unknown system flag %s", s)
		}
	}
	return s
}

func (p *parser) xflagList() (l []string) {
	p.xtake("(")
	if !p.hasPrefix(")") {
		l = append(l, p.xflag())
	}
	for !p.take(")") {
		p.xspace()
		l = append(l, p.xflag())
	}
	return
}

func (p *parser) xatom() string {
	return p.xtakechars(atomChar, "atom")
}

func (p *parser) xmailbox() string {
	s := p.xastring()
	// UTF-7 is deprecated in IMAP4rev2. IMAP4rev1 does not fully forbid
	// UTF-8 returned in mailbox names. We'll do our best by attempting to
	// decode utf-7. But if that doesn't work, we'll just use the original
	// string.
	// ../rfc/3501:964
	if !p.conn.enabled[capIMAP4rev2] {
		ns, err := utf7decode(s)
		if err != nil {
			p.conn.log.Infox("decoding utf7 or mailbox name", err, mlog.Field("name", s))
		} else {
			s = ns
		}
	}
	return s
}

// ../rfc/9051:6605
func (p *parser) xlistMailbox() string {
	if p.hasPrefix(`"`) || p.hasPrefix("{") {
		return p.xstring()
	}
	return p.xtakechars(atomChar+listWildcards+respSpecials, "list-char")
}

// ../rfc/9051:6707 ../rfc/9051:6848 ../rfc/5258:1095 ../rfc/5258:1169 ../rfc/5258:1196
func (p *parser) xmboxOrPat() ([]string, bool) {
	if !p.take("(") {
		return []string{p.xlistMailbox()}, false
	}
	l := []string{p.xlistMailbox()}
	for !p.take(")") {
		p.xspace()
		l = append(l, p.xlistMailbox())
	}
	return l, true
}

// ../rfc/9051:7056
// RECENT only in ../rfc/3501:5047
// APPENDLIMIT is from ../rfc/7889:252
func (p *parser) xstatusAtt() string {
	return p.xtakelist("MESSAGES", "UIDNEXT", "UIDVALIDITY", "UNSEEN", "DELETED", "SIZE", "RECENT", "APPENDLIMIT")
}

// ../rfc/9051:7133 ../rfc/9051:7034
func (p *parser) xnumSet() (r numSet) {
	defer p.context("numSet")()
	if p.take("$") {
		return numSet{searchResult: true}
	}
	r.ranges = append(r.ranges, p.xnumRange())
	for p.take(",") {
		r.ranges = append(r.ranges, p.xnumRange())
	}
	return r
}

// parse numRange, which can be just a setNumber.
func (p *parser) xnumRange() (r numRange) {
	if p.take("*") {
		r.first.star = true
	} else {
		r.first.number = p.xnznumber()
	}
	if p.take(":") {
		r.last = &setNumber{}
		if p.take("*") {
			r.last.star = true
		} else {
			r.last.number = p.xnznumber()
		}
	}
	return
}

// ../rfc/9051:6989 ../rfc/3501:4977
func (p *parser) xsectionMsgtext() (r *sectionMsgtext) {
	defer p.context("sectionMsgtext")()
	msgtextWords := []string{"HEADER.FIELDS.NOT", "HEADER.FIELDS", "HEADER", "TEXT"}
	w := p.xtakelist(msgtextWords...)
	r = &sectionMsgtext{s: w}
	if strings.HasPrefix(w, "HEADER.FIELDS") {
		p.xspace()
		p.xtake("(")
		r.headers = append(r.headers, textproto.CanonicalMIMEHeaderKey(p.xastring()))
		for {
			if p.take(")") {
				break
			}
			p.xspace()
			r.headers = append(r.headers, textproto.CanonicalMIMEHeaderKey(p.xastring()))
		}
	}
	return
}

// ../rfc/9051:6999 ../rfc/3501:4991
func (p *parser) xsectionSpec() (r *sectionSpec) {
	defer p.context("parseSectionSpec")()

	n, ok := p.nznumber()
	if !ok {
		return &sectionSpec{msgtext: p.xsectionMsgtext()}
	}
	defer p.context("part...")()
	pt := &sectionPart{}
	pt.part = append(pt.part, n)
	for {
		if !p.take(".") {
			break
		}
		if n, ok := p.nznumber(); ok {
			pt.part = append(pt.part, n)
			continue
		}
		if p.take("MIME") {
			pt.text = &sectionText{mime: true}
			break
		}
		pt.text = &sectionText{msgtext: p.xsectionMsgtext()}
		break
	}
	return &sectionSpec{part: pt}
}

// ../rfc/9051:6985 ../rfc/3501:4975
func (p *parser) xsection() *sectionSpec {
	defer p.context("parseSection")()
	p.xtake("[")
	if p.take("]") {
		return &sectionSpec{}
	}
	r := p.xsectionSpec()
	p.xtake("]")
	return r
}

// ../rfc/9051:6841
func (p *parser) xpartial() *partial {
	p.xtake("<")
	offset := p.xnumber()
	p.xtake(".")
	count := p.xnznumber()
	p.xtake(">")
	return &partial{offset, count}
}

// ../rfc/9051:6987
func (p *parser) xsectionBinary() (r []uint32) {
	p.xtake("[")
	if p.take("]") {
		return nil
	}
	r = append(r, p.xnznumber())
	for {
		if !p.take(".") {
			break
		}
		r = append(r, p.xnznumber())
	}
	p.xtake("]")
	return r
}

// ../rfc/9051:6557 ../rfc/3501:4751
func (p *parser) xfetchAtt() (r fetchAtt) {
	defer p.context("fetchAtt")()
	words := []string{
		"ENVELOPE", "FLAGS", "INTERNALDATE", "RFC822.SIZE", "BODYSTRUCTURE", "UID", "BODY.PEEK", "BODY", "BINARY.PEEK", "BINARY.SIZE", "BINARY",
		"RFC822.HEADER", "RFC822.TEXT", "RFC822", // older IMAP
	}
	f := p.xtakelist(words...)
	r.peek = strings.HasSuffix(f, ".PEEK")
	r.field = strings.TrimSuffix(f, ".PEEK")

	switch r.field {
	case "BODY":
		if p.hasPrefix("[") {
			r.section = p.xsection()
			if p.hasPrefix("<") {
				r.partial = p.xpartial()
			}
		}
	case "BINARY":
		r.sectionBinary = p.xsectionBinary()
		if p.hasPrefix("<") {
			r.partial = p.xpartial()
		}
	case "BINARY.SIZE":
		r.sectionBinary = p.xsectionBinary()
	}
	return
}

// ../rfc/9051:6553 ../rfc/3501:4748
func (p *parser) xfetchAtts() []fetchAtt {
	defer p.context("fetchAtts")()

	fields := func(l ...string) []fetchAtt {
		r := make([]fetchAtt, len(l))
		for i, s := range l {
			r[i] = fetchAtt{field: s}
		}
		return r
	}

	if w, ok := p.takelist("ALL", "FAST", "FULL"); ok {
		switch w {
		case "ALL":
			return fields("FLAGS", "INTERNALDATE", "RFC822.SIZE", "ENVELOPE")
		case "FAST":
			return fields("FLAGS", "INTERNALDATE", "RFC822.SIZE")
		case "FULL":
			return fields("FLAGS", "INTERNALDATE", "RFC822.SIZE", "ENVELOPE", "BODY")
		}
		panic("missing case")
	}

	if !p.hasPrefix("(") {
		return []fetchAtt{p.xfetchAtt()}
	}

	l := []fetchAtt{}
	p.xtake("(")
	for {
		l = append(l, p.xfetchAtt())
		if !p.take(" ") {
			break
		}
	}
	p.xtake(")")
	return l
}

func xint(p *parser, s string) int {
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		p.xerrorf("bad int %q: %v", s, err)
	}
	return int(v)
}

func (p *parser) digit() (string, bool) {
	if p.empty() {
		return "", false
	}
	c := p.orig[p.o]
	if c < '0' || c > '9' {
		return "", false
	}
	s := p.orig[p.o : p.o+1]
	p.o++
	return s, true
}

func (p *parser) xdigit() string {
	s, ok := p.digit()
	if !ok {
		p.xerrorf("expected digit")
	}
	return s
}

// ../rfc/9051:6492 ../rfc/3501:4695
func (p *parser) xdateDayFixed() int {
	if p.take(" ") {
		return xint(p, p.xdigit())
	}
	return xint(p, p.xdigit()+p.xdigit())
}

var months = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}

// ../rfc/9051:6495 ../rfc/3501:4698
func (p *parser) xdateMonth() time.Month {
	s := strings.ToLower(p.xtaken(3))
	for i, m := range months {
		if m == s {
			return time.Month(1 + i)
		}
	}
	p.xerrorf("unknown month %q", s)
	return 0
}

// ../rfc/9051:7120 ../rfc/3501:5067
func (p *parser) xtime() (int, int, int) {
	h := xint(p, p.xtaken(2))
	p.xtake(":")
	m := xint(p, p.xtaken(2))
	p.xtake(":")
	s := xint(p, p.xtaken(2))
	return h, m, s
}

// ../rfc/9051:7159 ../rfc/3501:5083
func (p *parser) xzone() (string, int) {
	sign := p.xtakelist("+", "-")
	s := p.xtaken(4)
	v := xint(p, s)
	seconds := (v/100)*3600 + (v%100)*60
	if sign[0] == '-' {
		seconds = -seconds
	}
	return sign + s, seconds
}

// ../rfc/9051:6502 ../rfc/3501:4713
func (p *parser) xdateTime() time.Time {
	// DQUOTE date-day-fixed "-" date-month "-" date-year SP time SP zone DQUOTE
	p.xtake(`"`)
	day := p.xdateDayFixed()
	p.xtake("-")
	month := p.xdateMonth()
	p.xtake("-")
	year := xint(p, p.xtaken(4))
	p.xspace()
	hours, minutes, seconds := p.xtime()
	p.xspace()
	name, zoneSeconds := p.xzone()
	p.xtake(`"`)
	loc := time.FixedZone(name, zoneSeconds)
	return time.Date(year, month, day, hours, minutes, seconds, 0, loc)
}

// ../rfc/9051:6655 ../rfc/7888:330 ../rfc/3501:4801
func (p *parser) xliteralSize(maxSize int64, lit8 bool) (size int64, sync bool) {
	// todo: enforce that we get non-binary when ~ isn't present?
	if lit8 {
		p.take("~")
	}
	p.xtake("{")
	size = p.xnumber64()
	if maxSize > 0 && size > maxSize {
		// ../rfc/7888:249
		line := fmt.Sprintf("* BYE [ALERT] Max literal size %d is larger than allowed %d in this context", size, maxSize)
		err := errors.New("literal too big")
		panic(syntaxError{line, "TOOBIG", err.Error(), err})
	}

	sync = !p.take("+")
	p.xtake("}")
	p.xempty()
	return size, sync
}

var searchKeyWords = []string{
	"ALL", "ANSWERED", "BCC",
	"BEFORE", "BODY",
	"CC", "DELETED", "FLAGGED",
	"FROM", "KEYWORD",
	"NEW", "OLD", "ON", "RECENT", "SEEN",
	"SINCE", "SUBJECT",
	"TEXT", "TO",
	"UNANSWERED", "UNDELETED", "UNFLAGGED",
	"UNKEYWORD", "UNSEEN",
	"DRAFT", "HEADER",
	"LARGER", "NOT",
	"OR",
	"SENTBEFORE", "SENTON",
	"SENTSINCE", "SMALLER",
	"UID", "UNDRAFT",
}

// ../rfc/9051:6923 ../rfc/3501:4957
// differences: rfc 9051 removes NEW, OLD, RECENT and makes SMALLER and LARGER number64 instead of number.
func (p *parser) xsearchKey() *searchKey {
	if p.take("(") {
		sk := p.xsearchKey()
		l := []searchKey{*sk}
		for !p.take(")") {
			p.xspace()
			l = append(l, *p.xsearchKey())
		}
		return &searchKey{searchKeys: l}
	}

	w, ok := p.takelist(searchKeyWords...)
	if !ok {
		seqs := p.xnumSet()
		return &searchKey{seqSet: &seqs}
	}

	sk := &searchKey{op: w}
	switch sk.op {
	case "ALL":
	case "ANSWERED":
	case "BCC":
		p.xspace()
		sk.astring = p.xastring()
	case "BEFORE":
		p.xspace()
		sk.date = p.xdate()
	case "BODY":
		p.xspace()
		sk.astring = p.xastring()
	case "CC":
		p.xspace()
		sk.astring = p.xastring()
	case "DELETED":
	case "FLAGGED":
	case "FROM":
		p.xspace()
		sk.astring = p.xastring()
	case "KEYWORD":
		p.xspace()
		sk.atom = p.xatom()
	case "NEW":
	case "OLD":
	case "ON":
		p.xspace()
		sk.date = p.xdate()
	case "RECENT":
	case "SEEN":
	case "SINCE":
		p.xspace()
		sk.date = p.xdate()
	case "SUBJECT":
		p.xspace()
		sk.astring = p.xastring()
	case "TEXT":
		p.xspace()
		sk.astring = p.xastring()
	case "TO":
		p.xspace()
		sk.astring = p.xastring()
	case "UNANSWERED":
	case "UNDELETED":
	case "UNFLAGGED":
	case "UNKEYWORD":
		p.xspace()
		sk.atom = p.xatom()
	case "UNSEEN":
	case "DRAFT":
	case "HEADER":
		p.xspace()
		sk.headerField = p.xastring()
		p.xspace()
		sk.astring = p.xastring()
	case "LARGER":
		p.xspace()
		sk.number = p.xnumber64()
	case "NOT":
		p.xspace()
		sk.searchKey = p.xsearchKey()
	case "OR":
		p.xspace()
		sk.searchKey = p.xsearchKey()
		p.xspace()
		sk.searchKey2 = p.xsearchKey()
	case "SENTBEFORE":
		p.xspace()
		sk.date = p.xdate()
	case "SENTON":
		p.xspace()
		sk.date = p.xdate()
	case "SENTSINCE":
		p.xspace()
		sk.date = p.xdate()
	case "SMALLER":
		p.xspace()
		sk.number = p.xnumber64()
	case "UID":
		p.xspace()
		sk.uidSet = p.xnumSet()
	case "UNDRAFT":
	default:
		p.xerrorf("missing case for op %q", sk.op)
	}
	return sk
}

// ../rfc/9051:6489 ../rfc/3501:4692
func (p *parser) xdateDay() int {
	d := p.xdigit()
	if s, ok := p.digit(); ok {
		d += s
	}
	return xint(p, d)
}

// ../rfc/9051:6487 ../rfc/3501:4690
func (p *parser) xdate() time.Time {
	dquote := p.take(`"`)
	day := p.xdateDay()
	p.xtake("-")
	mon := p.xdateMonth()
	p.xtake("-")
	year := xint(p, p.xtaken(4))
	if dquote {
		p.take(`"`)
	}
	return time.Date(year, mon, day, 0, 0, 0, 0, time.UTC)
}

// ../rfc/9051:7090 ../rfc/4466:716
func (p *parser) xtaggedExtLabel() string {
	return p.xtake1fn(func(i int, c rune) bool {
		return c >= 'A' && c <= 'Z' || c == '-' || c == '_' || c == '.' || i > 0 && (c >= '0' && c <= '9' || c == ':')
	})
}

// no return value since we don't currently use the value.
// ../rfc/9051:7111 ../rfc/4466:749
func (p *parser) xtaggedExtVal() {
	if p.take("(") {
		if p.take(")") {
			return
		}
		p.xtaggedExtComp()
		p.xtake(")")
	} else {
		p.xtaggedExtSimple()
	}
}

// ../rfc/9051:7109 ../rfc/4466:747
func (p *parser) xtaggedExtSimple() {
	s := p.digits()
	if s == "" {
		p.xnumSet()
	}

	// This can be a number64, or the start of a sequence-set. A sequence-set can also
	// start with a number, but only an uint32. After the number we'll try to continue
	// parsing as a sequence-set.
	_, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		p.xerrorf("parsing int: %v", err)
	}

	if p.take(":") {
		if !p.take("*") {
			p.xnznumber()
		}
	}
	for p.take(",") {
		p.xnumRange()
	}
}

// ../rfc/9051:7111 ../rfc/4466:735
func (p *parser) xtaggedExtComp() {
	if p.take("(") {
		p.xtaggedExtComp()
		p.xtake(")")
		return
	}
	p.xastring()
	for p.space() {
		p.xtaggedExtComp()
	}
}
