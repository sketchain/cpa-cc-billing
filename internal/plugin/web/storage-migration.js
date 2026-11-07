"use strict";

// The plugin was named cpa-cc-billing before. The page keeps its browser
// state under the new name's prefix, so its first load copies the old keys of
// this origin across once; the old keys stay, so the old plugin's page keeps
// working until it is removed.
(() => {
  const LEGACY_PREFIX = "cpa-cc-billing";
  const PREFIX = "cpa-super-billing";
  const MARKER = PREFIX + ".storage-migrated.v1";
  const migrate = (storageName) => {
    try {
      const storage = window[storageName];
      if (!storage || storage.getItem(MARKER)) return;
      const keys = [];
      for (let index = 0; index < storage.length; index++) {
        const key = storage.key(index);
        if (key && key.startsWith(LEGACY_PREFIX)) keys.push(key);
      }
      for (const key of keys) {
        const next = PREFIX + key.slice(LEGACY_PREFIX.length);
        if (storage.getItem(next) === null) storage.setItem(next, storage.getItem(key));
      }
      storage.setItem(MARKER, "1");
    } catch (_) {}
  };
  migrate("localStorage");
  migrate("sessionStorage");
})();
