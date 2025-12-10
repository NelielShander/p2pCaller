const user = `user_${Date.now()}`
const ws = new WebSocket(`/ws?room=1&peer=${user}`)
const pc = new RTCPeerConnection({'iceServers': [{'urls': 'stun:stun.l.google.com:19302'}]})

ws.onopen = async function () {
    await init()

    ws.onmessage = message => {
        const data = JSON.parse(message.data)
        switch (data.type) {
            case 'offer':
                createAnswer(data);
                break
            case 'answer':
                createSession(data);
                break
            case 'ice':
                addCandidate(data);
                break
        }

        pc.onicecandidate = event => {
            if (event.candidate) {
                message = {
                    type: 'ice', candidate: event.candidate
                }
                ws.send(JSON.stringify(message))
            }
        }

        pc.ontrack = event => {
            const remoteVideo = document.getElementById('remoteVideo');
            remoteVideo.srcObject = event.streams[0];
        };
    }

}

async function init() {
    const localVideo = document.getElementById('localVideo')
    const localStream = await navigator.mediaDevices.getUserMedia({'video': true, 'audio': true})

    localVideo.srcObject = localStream
    localStream.getTracks().forEach(track => {
        pc.addTrack(track, localStream)
    })
}

async function createOffer() {
    const offer = await pc.createOffer()

    await pc.setLocalDescription(offer)
    ws.send(JSON.stringify(offer))
}

async function createAnswer(data) {
    await pc.setRemoteDescription(new RTCSessionDescription(data))
    const answer = await pc.createAnswer()

    await pc.setLocalDescription(answer)
    ws.send(JSON.stringify(answer))
}

async function createSession(data) {
    await pc.setRemoteDescription(new RTCSessionDescription(data))
}

async function addCandidate(data) {
    await pc.addIceCandidate(new RTCIceCandidate(data.candidate))
}