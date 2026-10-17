import { createContext, useContext, useEffect, useState, type ReactNode } from "react";

export type ThemePref = "system" | "light" | "dark";

type Prefs = {
  theme: ThemePref;
  resolved: "light" | "dark";
  setTheme: (t: ThemePref) => void;
};

const PrefsContext = createContext<Prefs | null>(null);
const themes = ["system", "light", "dark"] as const;
const storageKey = "hangar.theme";

// A ?theme= query parameter wins over the saved choice, so a screenshot can
// ask for one theme without touching storage.
function initialTheme(): ThemePref {
  const fromQuery = new URLSearchParams(window.location.search).get("theme");
  if (fromQuery && (themes as readonly string[]).includes(fromQuery)) return fromQuery as ThemePref;
  const saved = localStorage.getItem(storageKey);
  if (saved && (themes as readonly string[]).includes(saved)) return saved as ThemePref;
  return "system";
}

function systemTheme(): "light" | "dark" {
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

/** Sets data-theme before the first render, so a dark page never flashes light. */
export function applyInitialTheme() {
  const t = initialTheme();
  document.documentElement.dataset.theme = t === "system" ? systemTheme() : t;
}

export function PrefsProvider({ children }: { children: ReactNode }) {
  const [theme, setTheme] = useState<ThemePref>(initialTheme);
  const [system, setSystem] = useState(systemTheme);

  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => setSystem(systemTheme());
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  const resolved = theme === "system" ? system : theme;

  useEffect(() => {
    document.documentElement.dataset.theme = resolved;
    localStorage.setItem(storageKey, theme);
  }, [theme, resolved]);

  return <PrefsContext.Provider value={{ theme, resolved, setTheme }}>{children}</PrefsContext.Provider>;
}

export function usePrefs() {
  const prefs = useContext(PrefsContext);
  if (!prefs) throw new Error("usePrefs outside PrefsProvider");
  return prefs;
}
