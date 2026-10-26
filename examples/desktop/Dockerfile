# A small X desktop for ksar run -display (see the README's Display section).
#
#   docker build -t ksar-desktop examples/desktop
#   docker create --name d ksar-desktop && docker export d > build/desktop.tar && docker rm d
#   ksar run -mem 1536M -timeout 30m -display 1280x800 -image build/desktop.tar -- \
#     sh -c 'Xorg :0 -nolisten tcp & sleep 2; dbus-launch --exit-with-session startxfce4'
FROM alpine:3.20
RUN apk add --no-cache xorg-server xf86-input-evdev xf86-input-libinput xf86-video-fbdev \
    xterm twm xsetroot xeyes font-dejavu font-misc-misc mesa-dri-gallium \
    xfce4 xfce4-terminal dbus-x11 adwaita-icon-theme
