docker_build('rtsp-app', '.', dockerfile='Dockerfile')
k8s_yaml('kubernetes.yaml')
k8s_resource('rtsp-app', port_forwards=[
    port_forward(1935, 1935, name='rtmp-server'),
    port_forward(8888, 8888, name='hls-server'),
    port_forward(8554, 8554, name='rtsp-server'),
    port_forward(9998, 9998, name='metrics-server')
])