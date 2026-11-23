package domain

const DocMarkdownDisplayVersion = 1

func docMarkdownWhitespace(character rune) bool {
	return character >= '\t' && character <= '\r' || character == ' ' || character == '\u00a0' || character == '\u1680' ||
		character >= '\u2000' && character <= '\u200a' || character == '\u2028' || character == '\u2029' ||
		character == '\u202f' || character == '\u205f' || character == '\u3000' || character == '\ufeff'
}
