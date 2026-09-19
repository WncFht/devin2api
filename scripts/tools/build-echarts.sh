#!/usr/bin/env bash
# build-echarts.sh — 重建面板的定制 echarts 包。
#
# 面板只用折线/柱状/饼图与少量组件，官方 echarts.min.js (~1.03MB) 大半用不上；
# 按 echarts 官方的按需引入方式以 esbuild 打包，产物 ~630KB。
#
# 用法: scripts/tools/build-echarts.sh [echarts版本，默认 5.6.0]
# 产物直接覆盖 internal/ccpanel/web/assets/js/echarts.min.js，提交进仓库——
# 单二进制 embed 发布链不引入前端构建步骤，此脚本只在升级版本时手动跑。
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
VER="${1:-5.6.0}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

cd "$WORK"
npm init -y >/dev/null
npm install "echarts@$VER" esbuild --no-audit --no-fund >/dev/null 2>&1

cat > entry.js <<'EOF'
// devin-2api 面板定制 echarts：只打包用到的图表与组件。
import * as echarts from 'echarts/core';
import { BarChart, LineChart, PieChart } from 'echarts/charts';
import {
  AxisPointerComponent,
  DataZoomInsideComponent,
  DataZoomSliderComponent,
  GraphicComponent,
  GridComponent,
  LegendComponent,
  MarkAreaComponent,
  MarkLineComponent,
  MarkPointComponent,
  TooltipComponent,
} from 'echarts/components';
import { CanvasRenderer } from 'echarts/renderers';

echarts.use([
  BarChart, LineChart, PieChart,
  AxisPointerComponent, DataZoomInsideComponent, DataZoomSliderComponent,
  GraphicComponent, GridComponent, LegendComponent,
  MarkAreaComponent, MarkLineComponent, MarkPointComponent, TooltipComponent,
  CanvasRenderer,
]);

window.echarts = echarts;
EOF

./node_modules/.bin/esbuild entry.js --bundle --minify --format=iife --outfile=echarts.min.js
cp echarts.min.js "$REPO/internal/ccpanel/web/assets/js/echarts.min.js"
echo "echarts.min.js rebuilt: echarts@$VER → $(du -h "$REPO/internal/ccpanel/web/assets/js/echarts.min.js" | cut -f1)"
