// The vendored libraries in one place: Preact, its hooks, and htm bound to
// Preact's h (templates are parsed at run time; no build step, no eval).
import { h, render, Fragment, Component } from '/vendor/preact-10.29.8/preact.module.js';
import { useState, useEffect, useRef, useMemo, useCallback, useLayoutEffect } from '/vendor/preact-10.29.8/hooks.module.js';
import htm from '/vendor/htm-3.1.1/htm.module.js';

export const html = htm.bind(h);
export { h, render, Fragment, Component, useState, useEffect, useRef, useMemo, useCallback, useLayoutEffect };
