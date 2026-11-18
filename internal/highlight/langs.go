package highlight

var (
	dq      = strDelim{open: `"`, close: `"`}
	sq      = strDelim{open: `'`, close: `'`}
	sqRaw   = strDelim{open: `'`, close: `'`, raw: true, multiline: true}
	bt      = strDelim{open: "`", close: "`", multiline: true}
	btRaw   = strDelim{open: "`", close: "`", multiline: true, raw: true}
	tripleD = strDelim{open: `"""`, close: `"""`, multiline: true}
	tripleS = strDelim{open: `'''`, close: `'''`, multiline: true}
	cBlock  = [][2]string{{"/*", "*/"}}
	slashes = []string{"//"}
	hash    = []string{"#"}
)

const cFamilyTypes = `int long short char float double boolean byte bool void string String
	int8 int16 int32 int64 uint8 uint16 uint32 uint64 size_t usize isize u8 u16 u32 u64 i8 i16 i32 i64
	f32 f64 str Vec Option Result Box Self Object Integer List Map`

var langs = map[string]*lang{
	"go": {name: "Go", line: slashes, block: cBlock, strs: []strDelim{dq, btRaw, sq},
		keywords: set(`break case chan const continue default defer else fallthrough for func go goto if
			import interface map package range return select struct switch type var`),
		consts: set(`true false nil iota`),
		types: set(`bool byte complex64 complex128 error float32 float64 int int8 int16 int32 int64 rune
			string uint uint8 uint16 uint32 uint64 uintptr any comparable`)},
	"js": {name: "JavaScript", line: slashes, block: cBlock, strs: []strDelim{dq, sq, bt}, at: true,
		keywords: set(`break case catch class const continue debugger default delete do else export extends
			finally for function if import in instanceof let new return super switch this throw try typeof var
			void while with yield async await of static get set interface type enum implements private public
			protected readonly declare namespace abstract as from keyof satisfies`),
		consts: set(`true false null undefined NaN Infinity`),
		types:  set(`string number boolean any unknown never object symbol bigint`)},
	"python": {name: "Python", line: hash, strs: []strDelim{tripleD, tripleS, dq, sq}, at: true,
		keywords: set(`and as assert async await break class continue def del elif else except finally for
			from global if import in is lambda nonlocal not or pass raise return try while with yield match case`),
		consts: set(`True False None self cls`),
		types:  set(`int float str bool list dict set tuple bytes object type`)},
	"shell": {name: "Shell", line: hash, strs: []strDelim{dq, sqRaw, bt}, vars: true,
		keywords: set(`if then else elif fi case esac for while until do done in function return local
			export readonly declare unset shift exit break continue set source alias`),
		consts: set(`true false`)},
	"rust": {name: "Rust", line: slashes, block: cBlock, strs: []strDelim{dq}, at: false,
		keywords: set(`as break const continue crate else enum extern fn for if impl in let loop match mod
			move mut pub ref return self Self static struct super trait type unsafe use where while async await dyn`),
		consts: set(`true false None Some Ok Err`), types: set(cFamilyTypes)},
	"c": {name: "C-family", line: slashes, block: cBlock, strs: []strDelim{tripleD, dq, sq}, at: true,
		keywords: set(`abstract assert break case catch class const continue default do else enum extends final
			finally for goto if implements import instanceof interface native new package private protected
			public return static super switch synchronized this throw throws transient try volatile while var val
			fun object when is in override open data sealed namespace using struct typedef sizeof include define
			ifdef ifndef endif pragma template typename auto inline extern register signed unsigned virtual delete
			friend operator union record async await internal readonly ref out let func guard protocol extension
			self lateinit companion suspend`),
		consts: set(`true false null nullptr NULL nil`), types: set(cFamilyTypes)},
	"php": {name: "PHP", line: []string{"//", "#"}, block: cBlock, strs: []strDelim{dq, sq}, vars: true,
		keywords: set(`abstract and array as break case catch class clone const continue declare default do echo
			else elseif empty extends final finally fn for foreach function global if implements include
			include_once instanceof interface isset list match namespace new or print private protected public
			require require_once return static switch throw trait try unset use var while yield`),
		consts: set(`true false null TRUE FALSE NULL`)},
	"ruby": {name: "Ruby", line: hash, strs: []strDelim{dq, sq}, vars: false,
		keywords: set(`def end if elsif else unless while until for in do return class module begin rescue
			ensure yield then case when require require_relative include extend self and or not puts
			attr_accessor attr_reader attr_writer lambda proc raise private protected public`),
		consts: set(`true false nil`)},
	"perl": {name: "Perl", line: hash, strs: []strDelim{dq, sq}, vars: true,
		keywords: set(`my our local sub if elsif else unless while until for foreach do return use require
			package last next redo print die`)},
	"lua": {name: "Lua", line: []string{"--"}, block: [][2]string{{"--[[", "]]"}}, strs: []strDelim{dq, sq},
		keywords: set(`and break do else elseif end for function goto if in local not or repeat return then until while`),
		consts:   set(`true false nil`)},
	"sql": {name: "SQL", line: []string{"--"}, block: cBlock, strs: []strDelim{sq, dq}, fold: true,
		keywords: set(`select from where and or not insert into values update set delete create table index drop
			alter add column primary key foreign references join left right inner outer full cross on group by order
			having limit offset as distinct union all is in like ilike between exists case when then else end
			default constraint unique begin commit rollback with returning view trigger function procedure
			grant revoke schema database if replace asc desc cascade check sequence`),
		consts: set(`null true false`),
		types:  set(`int integer bigint smallint serial bigserial text varchar char boolean bool date timestamp timestamptz numeric decimal real float jsonb json uuid bytea`)},
	"yaml": {name: "YAML", line: hash, strs: []strDelim{dq, sq}, keyLine: true,
		consts: set(`true false null yes no on off True False Null`)},
	"toml": {name: "TOML", line: hash, strs: []strDelim{tripleD, tripleS, dq, sq}, keyLine: true, sections: true,
		consts: set(`true false`)},
	"ini": {name: "Config", line: []string{"#", ";"}, strs: []strDelim{dq, sq}, keyLine: true, sections: true, vars: true,
		consts: set(`true false yes no on off`)},
	"json": {name: "JSON", line: slashes, block: cBlock, strs: []strDelim{dq}, jsonKeys: true,
		consts: set(`true false null`)},
	"css": {name: "CSS", line: slashes, block: cBlock, strs: []strDelim{dq, sq}, at: true, vars: true, hexColors: true,
		consts: set(`important inherit initial none auto`)},
	"dockerfile": {name: "Dockerfile", line: hash, strs: []strDelim{dq, sq}, vars: true,
		lineStart: set(`FROM RUN CMD LABEL EXPOSE ENV ADD COPY ENTRYPOINT VOLUME USER WORKDIR ARG ONBUILD
			STOPSIGNAL HEALTHCHECK SHELL MAINTAINER`),
		keywords: set(`AS`)},
	"make": {name: "Makefile", line: hash, strs: []strDelim{dq, sq}, keyLine: true, vars: true,
		keywords: set(`ifeq ifneq ifdef ifndef else endif include define endef export override`)},
	"markup": {name: "Markup", mode: modeMarkup},
	"diff":   {name: "Diff", mode: modeDiff},
}

var byExt = map[string]*lang{}

var byName = map[string]*lang{
	"makefile": langs["make"], "gnumakefile": langs["make"],
	"gemfile": langs["ruby"], "rakefile": langs["ruby"], "vagrantfile": langs["ruby"],
	".gitlab-ci.yml": langs["yaml"], ".gitignore": langs["ini"], ".gitattributes": langs["ini"],
	".editorconfig": langs["ini"], ".npmrc": langs["ini"], "go.mod": langs["go"], "go.sum": langs["ini"],
	".bashrc": langs["shell"], ".profile": langs["shell"], ".zshrc": langs["shell"], "pkgbuild": langs["shell"],
	"cmakelists.txt": langs["shell"], "jenkinsfile": langs["c"],
}

func init() {
	for exts, l := range map[string]string{
		"go": "go",
		"js mjs cjs jsx ts tsx mts cts vue svelte": "js",
		"py pyw pyi":                      "python",
		"sh bash zsh ksh fish bats envrc": "shell",
		"rs":                              "rust",
		"c h cc cpp cxx hpp hh hxx java kt kts scala groovy gradle cs swift m mm dart proto zig": "c",
		"php":             "php",
		"rb rake gemspec": "ruby",
		"pl pm":           "perl",
		"lua":             "lua",
		"sql":             "sql",
		"yml yaml":        "yaml",
		"toml":            "toml",
		"ini cfg conf properties env service timer socket desktop": "ini",
		"json jsonc json5 ipynb lock tfstate":                      "json",
		"css scss sass less":                                       "css",
		"html htm xhtml xml svg xsd xsl xslt plist csproj vbproj fsproj props targets resx config wsdl": "markup",
		"diff patch":   "diff",
		"mk":           "make",
		"tf hcl nomad": "ini",
	} {
		for _, e := range splitFields(exts) {
			byExt[e] = langs[l]
		}
	}
}

func splitFields(s string) []string {
	var out []string
	start := -1
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ' ' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	return out
}
