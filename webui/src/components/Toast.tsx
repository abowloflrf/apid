import { useEffect, useRef, useState } from "react";

type Listener = (msg: string) => void;
const listeners: Listener[] = [];

export function toast(msg: string): void {
  listeners.forEach((l) => l(msg));
}

export function ToastHost() {
  const [msg, setMsg] = useState<string | null>(null);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => {
    const listener: Listener = (m) => {
      setMsg(m);
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setMsg(null), 4000);
    };
    listeners.push(listener);
    return () => {
      const i = listeners.indexOf(listener);
      if (i >= 0) listeners.splice(i, 1);
      window.clearTimeout(timer.current);
    };
  }, []);

  return <div id="toast" className={`toast${msg ? " show" : ""}`}>{msg ?? ""}</div>;
}
