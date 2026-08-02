import { useEffect, useRef } from "react";
import { Chart, type ChartConfiguration } from "chart.js/auto";
import { C } from "../colors";

Chart.defaults.color = C.muted;
Chart.defaults.font.family = "ui-monospace, Menlo, Consolas, monospace";
Chart.defaults.font.size = 11;
Chart.defaults.maintainAspectRatio = false;

export function ChartCanvas({ config }: { config: ChartConfiguration }) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const chartRef = useRef<Chart | null>(null);

  useEffect(() => {
    if (!canvasRef.current) return;
    if (chartRef.current) {
      chartRef.current.data = config.data;
      if (config.options) chartRef.current.options = config.options;
      chartRef.current.update();
    } else {
      chartRef.current = new Chart(canvasRef.current, config);
    }
  }, [config]);

  useEffect(() => {
    return () => {
      chartRef.current?.destroy();
      chartRef.current = null;
    };
  }, []);

  return <canvas ref={canvasRef} />;
}
