import { Marked, type Tokens } from "marked";
import { esc } from "./ui-layout.js";

/**
 * A task description as HTML for the board: GitHub-flavoured Markdown (tables, task lists, code), where raw HTML
 * is shown as text and only http(s)/mailto links survive, so a description cannot run code in Mako's pages.
 * Relative links and GitLab uploads resolve against the issue's own page.
 */
export function renderMarkdown(md: string, issueUrl: string): string {
  const marked = new Marked({ gfm: true, breaks: true, async: false });
  marked.use({
    renderer: {
      html: ({ text }: Tokens.HTML | Tokens.Tag) => esc(text),
      link({ href, title, tokens }: Tokens.Link) {
        const text = this.parser.parseInline(tokens);
        const url = safeUrl(href, issueUrl);
        if (!url) return text;
        return `<a href="${esc(url)}"${title ? ` title="${esc(title)}"` : ""} target="_blank" rel="noreferrer">${text}</a>`;
      },
      image({ href, title, text }: Tokens.Image) {
        const url = safeUrl(href, issueUrl);
        if (!url || url.startsWith("mailto:")) return esc(text);
        return `<a href="${esc(url)}" target="_blank" rel="noreferrer"><img src="${esc(url)}" alt="${esc(text)}"${title ? ` title="${esc(title)}"` : ""} loading="lazy"></a>`;
      },
    },
  });
  return marked.parse(md.replace(/<!--[\s\S]*?-->/g, "")) as string;
}

function safeUrl(href: string, issueUrl: string): string | undefined {
  try {
    // GitLab writes attachments as "/uploads/…", relative to the project, not to the host.
    const project = issueUrl.replace(/\/-\/(issues|work_items)\/.*$/, "/");
    const url = href.startsWith("/uploads/") ? new URL(project.replace(/\/$/, "") + href) : new URL(href, issueUrl);
    return ["http:", "https:", "mailto:"].includes(url.protocol) ? url.href : undefined;
  } catch {
    return undefined;
  }
}
