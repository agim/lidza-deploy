import { describe, expect, it } from 'vitest';
import { helpArticles, searchHelp } from './help';

const text = (article: (typeof helpArticles)[number]) => [article.title, ...(article.keywords || []), ...article.body].join(' ');

describe('help articles', () => {
  it('have unique ids and non-empty bodies', () => {
    const ids = helpArticles.map(article => article.id);
    expect(new Set(ids).size).toBe(ids.length);
    for (const article of helpArticles) expect(article.body.join('').trim()).not.toBe('');
  });

  it('use only placeholder hostnames and documentation addresses', () => {
    for (const article of helpArticles) {
      const body = text(article);
      for (const host of body.match(/\b[a-z0-9-]+(?:\.[a-z0-9-]+)+\b/gi) || []) {
        if (/^\d+(\.\d+)*$/.test(host)) continue;
        expect(host, `${article.id}: ${host}`).toMatch(/(^|\.)example\.(com|org|net)$/i);
      }
      for (const address of body.match(/\b\d{1,3}(?:\.\d{1,3}){3}\b/g) || []) {
        expect(address, `${article.id}: ${address}`).toMatch(/^(192\.0\.2|198\.51\.100|203\.0\.113)\./);
      }
    }
  });

  it('finds articles by every word of the query', () => {
    expect(searchHelp('').length).toBe(helpArticles.length);
    expect(searchHelp('redirect').map(article => article.id)).toContain('redirect-domains');
    expect(searchHelp('restore deleted').map(article => article.id)).toContain('deleted-apps');
    expect(searchHelp('STARTTLS').map(article => article.id)).toEqual(['mail-tls']);
    expect(searchHelp('no such topic anywhere')).toEqual([]);
  });
});
