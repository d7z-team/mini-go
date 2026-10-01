const assert = require('node:assert/strict');
const fs = require('node:fs');
const test = require('node:test');
const textmate = require('vscode-textmate');
const oniguruma = require('vscode-oniguruma');

test('generated grammar tokenizes language facts without matching strings and comments', async () => {
    const wasm = fs.readFileSync(require.resolve('vscode-oniguruma/release/onig.wasm'));
    await oniguruma.loadWASM(wasm.buffer.slice(wasm.byteOffset, wasm.byteOffset + wasm.byteLength));
    const registry = new textmate.Registry({
        onigLib: Promise.resolve({ createOnigScanner: sources => new oniguruma.OnigScanner(sources), createOnigString: text => new oniguruma.OnigString(text) }),
        loadGrammar: async () => JSON.parse(fs.readFileSync(require.resolve('./syntaxes/mini-go.tmLanguage.json'), 'utf8')),
    });
    try {
        const grammar = await registry.loadGrammar('source.mgo');
        for (const [text, scope] of [
            ['chan', 'keyword.control.mgo'], ['select', 'keyword.control.mgo'],
            ['int64', 'storage.type.mgo'], ['complex', 'support.function.builtin.mgo'],
            ['0x1.fp2', 'constant.numeric.mgo'], ['"select"', 'string.quoted.double.mgo'],
            ["'界'", 'string.quoted.single.mgo'], ['// select', 'comment.line.double-slash.mgo'],
        ]) {
            const { tokens } = grammar.tokenizeLine(text, textmate.INITIAL);
            assert(tokens.some(token => token.scopes.includes(scope)), `${text}: ${JSON.stringify(tokens)}`);
            if (text.startsWith('"') || text.startsWith('//')) assert(tokens.every(token => !token.scopes.includes('keyword.control.mgo')));
        }
        for (const text of ['Int64', 'async', 'await']) {
            const { tokens } = grammar.tokenizeLine(text, textmate.INITIAL);
            assert(tokens.every(token => !token.scopes.some(scope => scope.startsWith('keyword.') || scope.startsWith('storage.type'))));
        }
        const method = 'func (b Box) Echo[T ~int](x T) T { return x }';
        const { tokens } = grammar.tokenizeLine(method, textmate.INITIAL);
        for (const [word, scope] of [['func', 'keyword.control.mgo'], ['int', 'storage.type.mgo'], ['return', 'keyword.control.mgo']]) {
            const start = method.indexOf(word);
            assert(tokens.some(token => token.startIndex <= start && token.endIndex >= start + word.length && token.scopes.includes(scope)), `${word}: ${JSON.stringify(tokens)}`);
        }
    } finally { registry.dispose(); }
});
