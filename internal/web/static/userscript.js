// ==UserScript==
// @name         BackTrackarr streaming scrobbler
// @namespace    https://github.com/DrSeussFreak/BackTrackarr
// @version      1.0
// @description  Sends what you watch on Netflix, Disney+, Hulu, Prime Video, Max, Paramount+, Peacock, Apple TV+, Crunchyroll and more to your BackTrackarr server.
// @match        https://www.netflix.com/*
// @match        https://www.disneyplus.com/*
// @match        https://www.hulu.com/*
// @match        https://www.primevideo.com/*
// @match        https://www.amazon.com/*
// @match        https://www.amazon.co.uk/*
// @match        https://www.amazon.de/*
// @match        https://www.amazon.ca/*
// @match        https://play.max.com/*
// @match        https://www.max.com/*
// @match        https://www.paramountplus.com/*
// @match        https://www.peacocktv.com/*
// @match        https://tv.apple.com/*
// @match        https://www.crunchyroll.com/*
// @match        https://www.discoveryplus.com/*
// @match        https://www.starz.com/*
// @match        https://www.mgmplus.com/*
// @match        https://www.amcplus.com/*
// @match        https://tubitv.com/*
// @match        https://pluto.tv/*
// @match        https://therokuchannel.roku.com/*
// @match        https://www.shudder.com/*
// @match        https://www.britbox.com/*
// @match        https://www.criterionchannel.com/*
// @match        https://mubi.com/*
// @grant        GM_xmlhttpRequest
// @connect      *
// @run-at       document-idle
// ==/UserScript==

// Best effort: streaming sites change their pages often. The script reads
// the site's own title/subtitle elements when it knows them, then the
// browser's Media Session info, then the page title, and lets BackTrackarr
// parse things like "S1:E3 Title" or "Season 1, Ep. 3".
(function () {
  "use strict";
  const SERVER = "{{SERVER}}";
  const TOKEN = "{{TOKEN}}";
  const USER = "{{USER}}"; // empty = BackTrackarr's default user

  const sites = [
    { host: /netflix\.com$/, service: "Netflix", title: ['[data-uia="video-title"] h4', '[data-uia="video-title"]'], sub: ['[data-uia="video-title"] span'] },
    { host: /disneyplus\.com$/, service: "Disney+", title: ['.title-field', '[data-testid="title-field"]'], sub: ['.subtitle-field', '[data-testid="subtitle-field"]'] },
    { host: /hulu\.com$/, service: "Hulu", title: ['.PlayerMetadata__titleText', '[data-automationid="player-title"]'], sub: ['.PlayerMetadata__subTitle', '[data-automationid="player-subtitle"]'] },
    { host: /(primevideo|amazon)\.[a-z.]+$/, service: "Prime Video", title: ['.atvwebplayersdk-title-text'], sub: ['.atvwebplayersdk-subtitle-text'] },
    { host: /max\.com$/, service: "Max", title: ['[data-testid="player-ux-asset-title"]'], sub: ['[data-testid="player-ux-season-episode"]', '[data-testid="player-ux-asset-subtitle"]'] },
    { host: /paramountplus\.com$/, service: "Paramount+", title: ['.title-header', '.video-title'], sub: ['.sub-title', '.episode-title'] },
    { host: /peacocktv\.com$/, service: "Peacock", title: ['.playback-header__title', '[data-testid="playback-title"]'], sub: ['.playback-header__subtitle', '[data-testid="playback-subtitle"]'] },
    { host: /tv\.apple\.com$/, service: "Apple TV+", title: [], sub: [] },
    { host: /crunchyroll\.com$/, service: "Crunchyroll", title: ['.show-title-link h4', '[data-t="show-title-link"]'], sub: ['.erc-current-media-info h1', 'h1.title'] },
    { host: /discoveryplus\.com$/, service: "Discovery+", title: [], sub: [] },
    { host: /starz\.com$/, service: "Starz", title: [], sub: [] },
    { host: /mgmplus\.com$/, service: "MGM+", title: [], sub: [] },
    { host: /amcplus\.com$/, service: "AMC+", title: [], sub: [] },
    { host: /tubitv\.com$/, service: "Tubi", title: [], sub: [] },
    { host: /pluto\.tv$/, service: "Pluto TV", title: [], sub: [] },
    { host: /roku\.com$/, service: "Roku Channel", title: [], sub: [] },
    { host: /shudder\.com$/, service: "Shudder", title: [], sub: [] },
    { host: /britbox\.com$/, service: "BritBox", title: [], sub: [] },
    { host: /criterionchannel\.com$/, service: "Criterion Channel", title: [], sub: [] },
    { host: /mubi\.com$/, service: "Mubi", title: [], sub: [] },
  ];
  const site = sites.find((s) => s.host.test(location.hostname)) || { service: location.hostname, title: [], sub: [] };

  const text = (sels) => {
    for (const s of sels) {
      const el = document.querySelector(s);
      const t = el && el.textContent && el.textContent.trim();
      if (t) return t;
    }
    return "";
  };

  function current() {
    const vids = [...document.querySelectorAll("video")].filter((v) => v.duration > 60 && isFinite(v.duration));
    if (!vids.length) return null;
    const v = vids.sort((a, b) => b.clientWidth * b.clientHeight - a.clientWidth * a.clientHeight)[0];
    let title = text(site.title);
    let sub = text(site.sub);
    const md = navigator.mediaSession && navigator.mediaSession.metadata;
    if (!title && md && md.title) {
      title = md.artist || md.album ? md.artist || md.album : md.title;
      if (md.artist || md.album) sub = md.title;
    }
    if (!title) {
      title = document.title.replace(/\s*[|\-–—]\s*(Netflix|Disney\+|Hulu|Prime Video|Max|Paramount\+|Peacock|Apple TV\+?|Crunchyroll|Watch.*)$/i, "").trim();
    }
    if (!title || title.length > 200) return null;
    return { v, title, sub };
  }

  function send(action, cur) {
    const body = {
      action, service: site.service, raw_title: cur.title, subtitle: cur.sub,
      position: cur.v.currentTime, duration: cur.v.duration, device: "Browser",
    };
    if (USER) body.user = USER;
    GM_xmlhttpRequest({
      method: "POST", url: SERVER + "/api/scrobble/" + TOKEN,
      headers: { "Content-Type": "application/json" }, data: JSON.stringify(body),
      onerror: () => console.warn("BackTrackarr: could not reach", SERVER),
    });
  }

  let last = null; // {key, state, watched}
  setInterval(() => {
    const cur = current();
    const key = cur ? cur.title + "|" + cur.sub : null;
    if (last && last.key !== key) {
      send("stop", last.cur); // switched title or left the player
      last = null;
    }
    if (!cur) return;
    const state = cur.v.paused ? "pause" : "start";
    const pct = (cur.v.currentTime * 100) / cur.v.duration;
    if (!last) last = { key, state: "", watched: false, cur };
    last.cur = cur;
    if (state !== last.state) {
      send(state, cur);
      last.state = state;
    }
    if (!last.watched && pct >= 90) {
      send("watched", cur);
      last.watched = true;
    }
  }, 10000);
  window.addEventListener("beforeunload", () => last && send("stop", last.cur));
})();
