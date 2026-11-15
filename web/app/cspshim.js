// Lets xterm.js run under the hub's content security policy, which allows
// no inline styles (see web/vendor/README.md). xterm.js's DOM renderer
// creates <style> elements and sets style attributes for 24-bit colours;
// the policy blocks both. Constructable stylesheets and CSSOM assignments
// are not inline styles, so:
//
//  - document.createElement('style') returns a <devc-style> element whose
//    text goes into a stylesheet adopted by the document while the element
//    is in it;
//  - setAttribute('style', text) sets style.cssText instead.
//
// The app itself never creates style elements or style attributes, so the
// shims change nothing else.

let installed = false;

export function installCspShim() {
  if (installed) return;
  installed = true;

  class DevcStyle extends HTMLElement {
    constructor() {
      super();
      this.sheet_ = new CSSStyleSheet();
      this.css_ = '';
    }
    connectedCallback() {
      if (!document.adoptedStyleSheets.includes(this.sheet_)) {
        document.adoptedStyleSheets = [...document.adoptedStyleSheets, this.sheet_];
      }
    }
    disconnectedCallback() {
      document.adoptedStyleSheets = document.adoptedStyleSheets.filter((s) => s !== this.sheet_);
    }
    get textContent() {
      return this.css_;
    }
    set textContent(v) {
      this.css_ = String(v ?? '');
      this.sheet_.replaceSync(this.css_);
    }
  }
  customElements.define('devc-style', DevcStyle);

  const createElement = Document.prototype.createElement;
  Document.prototype.createElement = function (tag, options) {
    if (typeof tag === 'string' && tag.toLowerCase() === 'style') return createElement.call(this, 'devc-style');
    return createElement.call(this, tag, options);
  };

  const setAttribute = Element.prototype.setAttribute;
  Element.prototype.setAttribute = function (name, value) {
    if (typeof name === 'string' && name.toLowerCase() === 'style' && this.style) {
      this.style.cssText = String(value);
      return;
    }
    return setAttribute.call(this, name, value);
  };
}
