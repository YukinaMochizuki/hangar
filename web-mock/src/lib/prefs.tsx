import { createContext, useContext, useEffect, useState, type ReactNode } from "react";

export type ThemePref = "system" | "light" | "dark";
export type Accent = "orange" | "blue" | "indigo";

export const accents: { id: Accent; label: string }[] = [
  { id: "orange", label: "Aviation orange" },
  { id: "blue", label: "Electric blue" },
  { id: "indigo", label: "Indigo" },
];

type Prefs = {
  theme: ThemePref;
  resolved: "light" | "dark";
  accent: Accent;
  setTheme: (t: ThemePref) => void;
  setAccent: (a: Accent) => void;
};

const PrefsContext = createContext<Prefs | null>(null);

// A ?theme= or ?accent= query parameter wins over the saved choice, so a
// screenshot can ask for one combination without touching storage.
function initial<T extends string>(key: string, allowed: readonly T[], fallback: T): T {
  const fromQuery = new URLSearchParams(window.location.search).get(key);
  if (fromQuery && (allowed as readonly string[]).includes(fromQuery)) return fromQuery as T;
  const saved = localStorage.getItem(`hangar.${key}`);
  if (saved && (allowed as readonly string[]).includes(saved)) return saved as T;
  return fallback;
}

function systemTheme(): "light" | "dark" {
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function PrefsProvider({ children }: { children: ReactNode }) {
  const [theme, setTheme] = useState<ThemePref>(() =>
    initial("theme", ["system", "light", "dark"] as const, "system"),
  );
  const [accent, setAccent] = useState<Accent>(() =>
    initial("accent", ["orange", "blue", "indigo"] as const, "orange"),
  );
  const [system, setSystem] = useState(systemTheme);

  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => setSystem(systemTheme());
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  const resolved = theme === "system" ? system : theme;

  useEffect(() => {
    const root = document.documentElement;
    root.dataset.theme = resolved;
    root.dataset.accent = accent;
    localStorage.setItem("hangar.theme", theme);
    localStorage.setItem("hangar.accent", accent);
  }, [theme, resolved, accent]);

  return (
    <PrefsContext.Provider value={{ theme, resolved, accent, setTheme, setAccent }}>
      {children}
    </PrefsContext.Provider>
  );
}

export function usePrefs() {
  const prefs = useContext(PrefsContext);
  if (!prefs) throw new Error("usePrefs outside PrefsProvider");
  return prefs;
}
