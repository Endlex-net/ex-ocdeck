/**
 * 内联 SVG 图标组件（tasks 2.3）。
 * 规格（brand-spec）：1.6px 描边、单色 currentColor、不引图标库。
 * 替换原文本字符图标：▶ ⏸ ▼ ▲ ⟳ ⋯ ▸ ▾ ⚠ ⓘ ⎇ 等。
 *
 * 所有图标 24x24 viewBox，描边 1.6px，stroke=currentColor，fill=none，
 * 默认通过 className 跟随父元素字号/颜色；尺寸由 CSS 控制（width/height: 1em 或显式类）。
 * 通过 aria-hidden 默认对辅助技术隐藏，文字标签由调用方提供（title/aria-label）。
 */

interface IconProps {
  className?: string;
  /** 显式无障碍标签，不传则 aria-hidden */
  title?: string;
}

function Svg({ className, title, children }: IconProps & { children: React.ReactNode }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      width="1em"
      height="1em"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.6}
      strokeLinecap="round"
      strokeLinejoin="round"
      role={title ? 'img' : undefined}
      aria-label={title}
      aria-hidden={title ? undefined : true}
      focusable="false"
    >
      {title ? <title>{title}</title> : null}
      {children}
    </svg>
  );
}

/** 激活 ▶ */
export function PlayIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M7 5.5L18 12 7 18.5z" />
    </Svg>
  );
}

/** 挂起 ⏸ */
export function PauseIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M9 5v14M15 5v14" />
    </Svg>
  );
}

/** 归档/收起 ▼ */
export function ArchiveIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M4 7h16v3H4zM5 10v9h14v-9M10 14h4" />
    </Svg>
  );
}

/** 恢复/展开 ▲ */
export function RestoreIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M12 5l6 6M12 5l-6 6M12 5v14" />
    </Svg>
  );
}

/** 重试 ⟳ */
export function RetryIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M4 12a8 8 0 1 1 2.5 5.8M4 18v-4h4" />
    </Svg>
  );
}

/** 更多 ⋯ */
export function MoreIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="6" cy="12" r="1.2" fill="currentColor" stroke="none" />
      <circle cx="12" cy="12" r="1.2" fill="currentColor" stroke="none" />
      <circle cx="18" cy="12" r="1.2" fill="currentColor" stroke="none" />
    </Svg>
  );
}

/** 展开标记 ▸（折叠态右侧三角） */
export function CaretRightIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M9 6l6 6-6 6" />
    </Svg>
  );
}

/** 展开标记 ▾（展开态下向三角） */
export function CaretDownIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M6 9l6 6 6-6" />
    </Svg>
  );
}

/** 警告 ⚠ */
export function WarnIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M12 4l9 16H3z" />
      <path d="M12 10v5M12 18v.5" />
    </Svg>
  );
}

/** 信息 ⓘ */
export function InfoIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 8v.5M12 11v6" />
    </Svg>
  );
}

/** 分支 ⎇ */
export function BranchIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="6" cy="6" r="2.5" />
      <circle cx="6" cy="18" r="2.5" />
      <circle cx="17" cy="8" r="2.5" />
      <path d="M6 8.5v7M17 10.5c0 4-5 3-5 6" />
    </Svg>
  );
}

/** 主题-暗色（月亮） */
export function MoonIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M20 14a8 8 0 1 1-10-10 6.5 6.5 0 0 0 10 10z" />
    </Svg>
  );
}

/** 主题-亮色（太阳） */
export function SunIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="12" cy="12" r="4" />
      <path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M18.4 5.6l-2.1 2.1M7.7 16.3l-2.1 2.1" />
    </Svg>
  );
}

/* 壳层导航图标（侧栏 / 命令面板，设计源 command-center.html） */

/** 指挥中心（活动条/折线图） */
export function HomeIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <polyline points="3 12 8 12 11 5 14 19 17 12 21 12" />
    </Svg>
  );
}

/** 项目管理（文件夹） */
export function FolderIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7z" />
    </Svg>
  );
}

/** 设置（滑块） */
export function SettingsIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <line x1="4" y1="7" x2="20" y2="7" />
      <circle cx="9" cy="7" r="2.2" />
      <line x1="4" y1="17" x2="20" y2="17" />
      <circle cx="15" cy="17" r="2.2" />
    </Svg>
  );
}

/** 命令面板入口（放大镜） */
export function SearchIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="11" cy="11" r="7" />
      <path d="m20 20-3.5-3.5" />
    </Svg>
  );
}

/** 新建（加号） */
export function PlusIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <line x1="12" y1="5" x2="12" y2="19" />
      <line x1="5" y1="12" x2="19" y2="12" />
    </Svg>
  );
}

/** 折叠侧栏（箭头指向左） */
export function SidebarCollapseIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <rect x="3" y="4" width="18" height="16" rx="2" />
      <path d="M9 4v16" />
      <path d="m14 9-3 3 3 3" />
    </Svg>
  );
}

/** 展开侧栏（箭头指向右） */
export function SidebarExpandIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <rect x="3" y="4" width="18" height="16" rx="2" />
      <path d="M9 4v16" />
      <path d="m12 9 3 3-3 3" />
    </Svg>
  );
}