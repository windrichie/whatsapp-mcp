import os
import subprocess
import tempfile

# Only real audio/video container demuxers may read the input. This stops a crafted
# playlist (.m3u8, concat, ...) from pulling other local files into the voice message.
_ALLOWED_INPUT_FORMATS = "mp3,wav,ogg,flac,mov,aac,matroska,amr,caf,aiff,w64"
_FFMPEG_TIMEOUT_SECONDS = 120

def convert_to_opus_ogg(input_file, output_file=None, bitrate="32k", sample_rate=24000):
    """
    Convert an audio file to Opus format in an Ogg container.

    Args:
        input_file (str): Path to the input audio file
        output_file (str, optional): Path to save the output file. If None, replaces the
                                    extension of input_file with .ogg
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)

    Returns:
        str: Path to the converted file

    Raises:
        FileNotFoundError: If the input file doesn't exist
        RuntimeError: If the ffmpeg conversion fails
    """
    if not os.path.isfile(input_file):
        raise FileNotFoundError(f"Input file not found: {input_file}")

    # If no output file is specified, replace the extension with .ogg
    if output_file is None:
        output_file = os.path.splitext(input_file)[0] + ".ogg"

    # Ensure the output directory exists
    output_dir = os.path.dirname(output_file)
    if output_dir and not os.path.exists(output_dir):
        os.makedirs(output_dir, mode=0o700)

    # Build the ffmpeg command. The "file:" prefix stops ffmpeg from treating the
    # path as a protocol URL (http:, concat:, ...), and the whitelists restrict what
    # the input may reference.
    cmd = [
        "ffmpeg",
        "-nostdin",
        "-hide_banner",
        "-loglevel", "error",
        "-protocol_whitelist", "file",
        "-format_whitelist", _ALLOWED_INPUT_FORMATS,
        "-i", "file:" + os.path.abspath(input_file),
        "-vn",                       # Audio only
        "-map_metadata", "-1",       # Do not copy tags (names, locations) from the source file
        "-c:a", "libopus",
        "-b:a", bitrate,
        "-ar", str(sample_rate),
        "-application", "voip",  # Optimize for voice
        "-vbr", "on",           # Variable bitrate
        "-compression_level", "10",  # Maximum compression
        "-frame_duration", "60",     # 60ms frames (good for voice)
        "-y",                        # Overwrite output file if it exists
        "file:" + os.path.abspath(output_file)
    ]

    try:
        # Run the ffmpeg command and capture output
        subprocess.run(
            cmd,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            check=True,
            timeout=_FFMPEG_TIMEOUT_SECONDS,
        )
        return output_file
    except FileNotFoundError:
        raise RuntimeError("ffmpeg is not installed or not on PATH")
    except subprocess.TimeoutExpired:
        raise RuntimeError(f"Audio conversion took longer than {_FFMPEG_TIMEOUT_SECONDS} seconds")
    except subprocess.CalledProcessError as e:
        raise RuntimeError(f"Failed to convert audio. You likely need to install ffmpeg {e.stderr[-2000:]}")


def convert_to_opus_ogg_temp(input_file, bitrate="32k", sample_rate=24000):
    """
    Convert an audio file to Opus format in an Ogg container and store in a temporary file.

    Args:
        input_file (str): Path to the input audio file
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)

    Returns:
        str: Path to the temporary file with the converted audio

    Raises:
        FileNotFoundError: If the input file doesn't exist
        RuntimeError: If the ffmpeg conversion fails
    """
    # Create a temporary file with .ogg extension
    temp_file = tempfile.NamedTemporaryFile(suffix=".ogg", delete=False)
    temp_file.close()

    try:
        # Convert the audio
        convert_to_opus_ogg(input_file, temp_file.name, bitrate, sample_rate)
        return temp_file.name
    except Exception as e:
        # Clean up the temporary file if conversion fails
        if os.path.exists(temp_file.name):
            os.unlink(temp_file.name)
        raise e


if __name__ == "__main__":
    # Example usage
    import sys

    if len(sys.argv) < 2:
        print("Usage: python audio.py input_file [output_file]")
        sys.exit(1)

    input_file = sys.argv[1]

    try:
        result = convert_to_opus_ogg_temp(input_file)
        print(f"Successfully converted to: {result}")
    except Exception as e:
        print(f"Error: {e}")
        sys.exit(1)
