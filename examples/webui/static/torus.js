// Circulating poloidal and toroidal currents, projected from the same surface.
// The brand mark uses a sparser mesh so the motion reads at icon size.
export function animateTorus(canvas, mini = false) {
  const ctx = canvas.getContext('2d'),
    motion = matchMedia('(prefers-reduced-motion: reduce)');
  let raf = 0,
    visible = false,
    last = -100,
    time = 0,
    aimX = 0,
    aimY = 0,
    leanX = 0,
    leanY = 0;
  canvas.addEventListener('pointermove', (e) => {
    const r = canvas.getBoundingClientRect();
    aimX = (2 * (e.clientX - r.left)) / r.width - 1;
    aimY = (2 * (e.clientY - r.top)) / r.height - 1;
  });
  canvas.addEventListener('pointerleave', () => {
    aimX = aimY = 0;
  });
  const tau = 2 * Math.PI;
  function draw(t) {
    const w = canvas.clientWidth,
      h = canvas.clientHeight;
    if (!w || !h) return;
    const dpr = Math.min(2, devicePixelRatio);
    if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
      canvas.width = Math.round(w * dpr);
      canvas.height = Math.round(h * dpr);
    }
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    ctx.translate(w / 2, h / 2);
    const scale = Math.min(w / 2.86, h / 2.34),
      ink = getComputedStyle(canvas).getPropertyValue('--accent').trim();
    leanX += (aimX - leanX) * 0.14;
    leanY += (aimY - leanY) * 0.14;
    const ease = motion.matches ? 0 : Math.exp(-2.4 * t);
    const tilt = 0.72 + 0.36 * Math.sin(t * 0.7) - 0.34 * leanY + 0.52 * ease,
      twist = -0.36 + 0.28 * Math.sin(t * 0.55) + 0.34 * leanX - 0.95 * ease,
      ca = Math.cos(twist),
      sa = Math.sin(twist);
    function point(u, v) {
      const phase = u * 3 - t * 1.7,
        minor = 0.285 + 0.062 * Math.sin(phase);
      const r = 0.91 + minor * Math.cos(v),
        x = r * Math.cos(u);
      const z = minor * Math.sin(v) + 0.095 * Math.sin(phase);
      const y = r * Math.sin(u) * Math.sin(tilt) - z * Math.cos(tilt);
      return [
        (x * ca - y * sa) * scale,
        (x * sa + y * ca) * scale,
        Math.sin(v) * Math.sin(tilt) + Math.cos(v) * Math.sin(u) * Math.cos(tilt),
      ];
    }
    function stroke(fn, steps, opacity, weight, frontOnly = true) {
      ctx.beginPath();
      let pen = false;
      for (let i = 0; i <= steps; i++) {
        const [x, y, z] = fn(i / steps);
        if (frontOnly && z < 0) {
          pen = false;
          continue;
        }
        if (pen) ctx.lineTo(x, y);
        else ctx.moveTo(x, y);
        pen = true;
      }
      ctx.globalAlpha = opacity;
      ctx.lineWidth = weight;
      ctx.stroke();
    }
    ctx.strokeStyle = ctx.fillStyle = ink;
    ctx.lineCap = 'round';
    const rings = mini ? 14 : 44,
      lanes = mini ? 5 : 15;
    for (let i = 0; i < rings; i++) {
      const u = (i * tau) / rings + t * 0.22;
      stroke(
        (f) => point(u, tau * f),
        mini ? 20 : 45,
        0.3 + 0.35 * (1 + Math.cos(u - t)) * 0.5,
        mini ? 1.05 : 0.9,
      );
    }
    for (let i = 0; i < lanes; i++)
      stroke(
        (f) => point(tau * f, (i * tau) / lanes + 0.16 * Math.sin(3 * tau * f - t)),
        mini ? 42 : 120,
        0.54,
        mini ? 0.85 : 0.85,
      );
    // Two luminous paths wind through the moving mesh, disappearing behind it.
    for (let k = 0; k < (mini ? 1 : 3); k++) {
      const head = t * 0.85 + (k * tau) / 3,
        v = (u) => 2 * u - t * 0.41 + (k * tau) / 3;
      stroke(
        (f) => {
          const u = head - (1 - f) * 1.45;
          return point(u, v(u));
        },
        36,
        0.94,
        mini ? 1.3 : 1.9,
      );
      const [x, y, z] = point(head, v(head));
      if (z > 0) {
        ctx.globalAlpha = 1;
        ctx.beginPath();
        ctx.arc(x, y, mini ? 1.55 : 2.6, 0, tau);
        ctx.fill();
      }
    }
    ctx.globalAlpha = 1;
  }
  function frame(now) {
    raf = 0;
    if (!visible || document.hidden) return;
    if (now - last >= 1000 / (mini ? 20 : 32)) {
      time += Math.min(0.08, (now - last) / 1000);
      last = now;
      draw(motion.matches ? 0 : time);
    }
    if (!motion.matches) raf = requestAnimationFrame(frame);
  }
  function sync() {
    cancelAnimationFrame(raf);
    last = performance.now() - 60;
    raf = requestAnimationFrame(frame);
  }
  new IntersectionObserver(([e]) => {
    visible = e.isIntersecting;
    sync();
  }).observe(canvas);
  new ResizeObserver(sync).observe(canvas);
  new MutationObserver(sync).observe(document.documentElement, {
    attributes: true,
    attributeFilter: ['data-theme'],
  });
  motion.addEventListener('change', sync);
  document.addEventListener('visibilitychange', sync);
}
