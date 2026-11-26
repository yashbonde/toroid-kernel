import MarkdownIt from 'markdown-it';
import { Schema, Slice } from 'prosemirror-model';
import { EditorState, TextSelection } from 'prosemirror-state';
import { EditorView } from 'prosemirror-view';
import {
  baseKeymap,
  chainCommands,
  lift,
  setBlockType,
  toggleMark,
  wrapIn,
} from 'prosemirror-commands';
import { history, undo, redo } from 'prosemirror-history';
import { keymap } from 'prosemirror-keymap';
import { wrapInList, splitListItem, liftListItem } from 'prosemirror-schema-list';
import {
  schema as common,
  defaultMarkdownSerializer,
  defaultMarkdownParser,
  MarkdownParser,
  MarkdownSerializer,
} from 'prosemirror-markdown';

// Generated HTML comes only from MarkdownIt's built-in renderer. Raw HTML and
// image fetching are disabled; links stay explicit user actions.
const markdown = new MarkdownIt({ html: false, breaks: true }).disable('image');
markdown.renderer.rules.link_open = (tokens, i, options, env, renderer) => {
  tokens[i].attrSet('rel', 'noopener noreferrer');
  tokens[i].attrSet('target', '_blank');
  return renderer.renderToken(tokens, i, options);
};
export function renderMarkdown(text) {
  return markdown.render(text);
}

const safeLink = (url) => /^(https?:\/\/|mailto:)/i.test(url);
const marks = common.spec.marks
  .update('link', {
    ...common.spec.marks.get('link'),
    parseDOM: [
      { tag: 'a[href]', getAttrs: (el) => (safeLink(el.href) ? { href: el.href } : false) },
    ],
  })
  .addToEnd('strike', { parseDOM: [{ tag: 's' }, { tag: 'del' }], toDOM: () => ['s', 0] });
const schema = new Schema({
  nodes: common.spec.nodes.remove('image').update('heading', {
    ...common.spec.nodes.get('heading'),
    content: 'text*',
  }),
  marks,
});
const parser = new MarkdownParser(
  schema,
  new MarkdownIt('commonmark', { html: false }).enable('strikethrough').disable('image'),
  {
    ...Object.fromEntries(
      Object.entries(defaultMarkdownParser.tokens).filter(([k]) => k !== 'image'),
    ),
    s: { mark: 'strike' },
  },
);
const serializer = new MarkdownSerializer(defaultMarkdownSerializer.nodes, {
  ...defaultMarkdownSerializer.marks,
  strike: { open: '~~', close: '~~', mixable: true, expelEnclosingWhitespace: true },
});

export function makeEditor(host, toolbar, onChange, onSend) {
  const { nodes, marks } = schema;
  function exitFinalCode(direction) {
    return (state, dispatch, view) => {
      const { $cursor } = state.selection;
      if (!$cursor || $cursor.parent.type !== nodes.code_block) return false;
      const end = state.doc.content.size;
      if ($cursor.end() + $cursor.depth !== end) return false;
      if (
        direction === 'right'
          ? $cursor.parentOffset !== $cursor.parent.content.size
          : !view.endOfTextblock('down', state)
      )
        return false;
      const tr = state.tr.insert(end, nodes.paragraph.create());
      dispatch(tr.setSelection(TextSelection.create(tr.doc, end + 1)).scrollIntoView());
      return true;
    };
  }
  const submit = () => {
    onSend();
    return true;
  };
  const keys = {
    'Mod-b': toggleMark(marks.strong),
    'Mod-i': toggleMark(marks.em),
    'Mod-z': undo,
    'Mod-y': redo,
    'Shift-Mod-z': redo,
    'Shift-Enter': submit,
    'Mod-Enter': submit,
    ArrowRight: exitFinalCode('right'),
    ArrowDown: exitFinalCode('down'),
    Enter: chainCommands(splitListItem(nodes.list_item), baseKeymap.Enter),
  };
  const plugins = [history(), keymap(keys), keymap(baseKeymap)];
  const fresh = () => EditorState.create({ schema, plugins });
  const editor = new EditorView(host, {
    state: fresh(),
    attributes: {
      role: 'textbox',
      'aria-multiline': 'true',
      'aria-label': 'Message',
      spellcheck: 'true',
    },
    handlePaste(v, e) {
      const text = e.clipboardData?.getData('text/plain');
      if (
        !text ||
        e.clipboardData.getData('text/html') ||
        v.state.selection.$from.parent.type.spec.code
      )
        return false;
      if (!/\*\*|\x60|^#{1,6} |^[-*>] |^\d+\. |\]\(/m.test(text)) return false;
      const doc = parser.parse(text);
      v.dispatch(v.state.tr.replaceSelection(Slice.maxOpen(doc.content)).scrollIntoView());
      return true;
    },
    dispatchTransaction(tr) {
      const next = this.state.apply(tr);
      this.updateState(next);
      if (tr.docChanged) onChange(serializer.serialize(next.doc));
      markState();
    },
  });
  function markState() {
    const { selection, storedMarks } = editor.state;
    for (const b of toolbar.querySelectorAll('[data-mark]')) {
      const mark = marks[b.dataset.mark];
      const active = selection.empty
        ? mark.isInSet(storedMarks || selection.$from.marks())
        : editor.state.doc.rangeHasMark(selection.from, selection.to, mark);
      b.setAttribute('aria-pressed', String(Boolean(active)));
    }
    host.classList.toggle(
      'blank',
      editor.state.doc.childCount === 1 && !editor.state.doc.firstChild.content.size,
    );
  }
  const run = (cmd) => {
    cmd(editor.state, editor.dispatch, editor);
    editor.focus();
  };
  function formatIcon(title) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    const use = document.createElementNS(svg.namespaceURI, 'use');
    use.setAttribute('href', '#i-' + title.toLowerCase().replaceAll(' ', '-'));
    svg.setAttribute('aria-hidden', 'true');
    svg.append(use);
    return svg;
  }
  const buttons = [
    ['Bold', toggleMark(marks.strong), 'strong'],
    ['Italic', toggleMark(marks.em), 'em'],
    ['Strikethrough', toggleMark(marks.strike), 'strike'],
    ['Inline code', toggleMark(marks.code), 'code'],
    ['Bulleted list', chainCommands(wrapInList(nodes.bullet_list), liftListItem(nodes.list_item))],
    ['Numbered list', chainCommands(wrapInList(nodes.ordered_list), liftListItem(nodes.list_item))],
    [
      'Block quote',
      (s, d) => {
        const p = s.selection.$from;
        for (let i = p.depth; i > 0; i--)
          if (p.node(i).type === nodes.blockquote) return lift(s, d);
        return wrapIn(nodes.blockquote)(s, d);
      },
    ],
    [
      'Code block',
      (s, d) =>
        setBlockType(
          s.selection.$from.parent.type === nodes.code_block ? nodes.paragraph : nodes.code_block,
        )(s, d),
    ],
  ];
  for (const [title, cmd, mark] of buttons) {
    const b = document.createElement('button');
    b.type = 'button';
    b.append(formatIcon(title));
    b.title = title;
    b.setAttribute('aria-label', title);
    if (mark) b.dataset.mark = mark;
    b.onmousedown = (e) => e.preventDefault();
    b.onclick = () => run(cmd);
    toolbar.append(b);
  }
  const link = document.createElement('button');
  link.type = 'button';
  link.append(formatIcon('Insert link'));
  link.title = 'Insert link';
  link.setAttribute('aria-label', link.title);
  const dialog = document.getElementById('link-dialog'),
    input = dialog.querySelector('input');
  link.onmousedown = (e) => e.preventDefault();
  link.onclick = () => {
    input.value = '';
    input.setCustomValidity('');
    dialog.showModal();
    input.focus();
  };
  dialog.querySelector('form').onsubmit = (e) => {
    e.preventDefault();
    const href = input.value.trim();
    if (!safeLink(href)) {
      input.setCustomValidity('Use an http, https, or mailto link.');
      input.reportValidity();
      return;
    }
    const { from, to } = editor.state.selection;
    let tr = editor.state.tr;
    if (from === to) tr = tr.insertText(href, from);
    tr.addMark(from, from === to ? from + href.length : to, marks.link.create({ href }));
    editor.dispatch(tr);
    dialog.close();
    editor.focus();
  };
  input.oninput = () => input.setCustomValidity('');
  toolbar.append(link);
  markState();
  return {
    focus: () => editor.focus(),
    snapshot: () => editor.state,
    restore(state) {
      editor.updateState(state || fresh());
      markState();
    },
    clear() {
      editor.updateState(fresh());
      markState();
    },
  };
}
