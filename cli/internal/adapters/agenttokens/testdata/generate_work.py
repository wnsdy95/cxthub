"""Regenerate synthetic work.json with official tiktoken==0.12.0 (offline cache OK)."""
import json
from pathlib import Path
import tiktoken

assert tiktoken.__version__ == "0.12.0"
units = [
    ("minified_json", '{"items":[', '{"path":"src/a1.go","sha":"a1b2c3d4","ok":true},', 5000, '{}]}'),
    ("digits", "", "1234567890", 1000, ""),
    ("marks_punctuation", "", "\u0301!", 8192, ""),
    ("url_query", "", "https://example.test/a1/b2?x=12&y=z#fragment;", 1500, ""),
    ("adjacent_tools", "", '{"tool":"exec","input":{"cmd":"go test ./..."},"output":"ok\\n"}', 1200, ""),
    ("case_contractions", "", "aB2can't<|endoftext|>\u4f60\u597d!", 1000, ""),
    ("whitespace_lookahead", "", " \t\r\n foo\tbar   \n/\n", 1000, ""),
]
cases = []
for encoding in ["o200k_base", "cl100k_base"]:
    counter = tiktoken.get_encoding(encoding)
    for name, prefix, unit, repeat, suffix in units:
        # cl100k treats adjacent marks/punctuation as one genuine long piece.
        # Keep its positive fixture bounded; the negative case is tested in Go.
        if encoding == "cl100k_base" and name == "marks_punctuation":
            unit += "a"
        text = prefix + unit * repeat + suffix
        cases.append(dict(encoding=encoding, name=name, prefix=prefix, unit=unit,
                          repeat=repeat, suffix=suffix,
                          tokens=len(counter.encode_ordinary(text)), bytes=len(text.encode())))
Path(__file__).with_name("work.json").write_text(json.dumps({
    "reference": "openai/tiktoken 0.12.0 encode_ordinary; synthetic repeated fixtures",
    "cases": cases,
}, ensure_ascii=True, indent=2) + "\n")
