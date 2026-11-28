import assert from "node:assert/strict";
import test from "node:test";
import {
  computeLatestSeq,
  computeUnreadSummary,
  nextReadSeqOnStick,
  readStoredReadSeq,
  writeStoredReadSeq,
} from "../src/lib/unreadTracker.ts";

function makeMemoryStorage() {
  const map = new Map();
  return {
    getItem(k) {
      return map.has(k) ? map.get(k) : null;
    },
    setItem(k, v) {
      map.set(k, String(v));
    },
  };
}

test("unreadTracker persists readSeq per thread and gates advancement on superReady", () => {
  const storage = makeMemoryStorage();
  assert.equal(readStoredReadSeq("chat:main", storage), 0);

  // Must NOT advance readSeq while displaying SWR snapshot (!superReady).
  assert.equal(
    nextReadSeqOnStick({ superReady: false, currentReadSeq: 10, latestSeq: 25 }),
    10
  );
  assert.equal(
    nextReadSeqOnStick({ superReady: true, currentReadSeq: 10, latestSeq: 25 }),
    25
  );
  // Monotonic: never regresses if an older snapshot arrives.
  assert.equal(
    nextReadSeqOnStick({ superReady: true, currentReadSeq: 25, latestSeq: 20 }),
    25
  );

  writeStoredReadSeq("chat:main", 25, storage);
  writeStoredReadSeq("wecom:g1", 8, storage);
  assert.equal(readStoredReadSeq("chat:main", storage), 25);
  assert.equal(readStoredReadSeq("wecom:g1", storage), 8);
});

test("computeUnreadSummary implements two-stage unread -> bottom -> none state machine", () => {
  const entries = [
    { id: "e1", seq: 10, role: "user" },
    { id: "e2", seq: 11, role: "corgi" },
    { id: "e3", seq: 12, role: "user" },
    { id: "e4", seq: 13, role: "corgi" },
    { id: "e5", seq: 14, role: "event" },
    { id: "e6", seq: 15, role: "corgi" },
  ];
  assert.equal(computeLatestSeq(entries), 15);

  // Scrolled up with 2 new corgi replies after readSeq=11 -> Stage 1 ("unread").
  const s1 = computeUnreadSummary({
    entries,
    readSeq: 11,
    userScrolledUp: true,
    showJumpBottom: true,
    firstUnreadPassed: false,
    hasEarlier: false,
  });
  assert.equal(s1.unreadCount, 2);
  assert.equal(s1.firstUnreadId, "e4");
  assert.equal(s1.firstUnreadSeq, 13);
  assert.equal(s1.jumpStage, "unread");
  assert.equal(s1.windowTruncated, false);

  // After jumping to / scrolling past first unread, keep unreadCount intact but collapse pill to Stage 2 ("bottom").
  const s2 = computeUnreadSummary({
    entries,
    readSeq: 11,
    userScrolledUp: true,
    showJumpBottom: true,
    firstUnreadPassed: true,
    hasEarlier: false,
  });
  assert.equal(s2.unreadCount, 2);
  assert.equal(s2.firstUnreadId, "e4");
  assert.equal(s2.jumpStage, "bottom");

  // Stuck at bottom -> Stage "none".
  const s3 = computeUnreadSummary({
    entries,
    readSeq: 15,
    userScrolledUp: false,
    showJumpBottom: false,
    firstUnreadPassed: false,
    hasEarlier: false,
  });
  assert.equal(s3.unreadCount, 0);
  assert.equal(s3.jumpStage, "none");

  // Cursor older than loaded window with more earlier history flags windowTruncated.
  const s4 = computeUnreadSummary({
    entries,
    readSeq: 5,
    userScrolledUp: true,
    showJumpBottom: true,
    firstUnreadPassed: false,
    hasEarlier: true,
  });
  assert.equal(s4.unreadCount, 3);
  assert.equal(s4.firstUnreadId, "e2");
  assert.equal(s4.windowTruncated, true);
  assert.equal(s4.jumpStage, "unread");
});
